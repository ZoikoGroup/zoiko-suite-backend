// Package store provides the PostgreSQL implementation of accounts-receivable-svc's
// persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policies in deployments/migrations are
// real and correctly written. But every method ALSO filters explicitly by
// tenant_id in its own SQL, rather than relying on RLS alone: this pool
// connects as a Postgres superuser by default (DB_USER=postgres), and Postgres
// superusers unconditionally bypass Row-Level Security regardless of policy.
// The explicit predicate is what isolates tenants today (see migration 000003's
// comment for the full reasoning — it is the same defect general-ledger-svc hit
// in CI).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

// constraintCustomerInvoiceNumber is the UNIQUE (tenant_id, customer_id,
// invoice_number) constraint from 000001. Declared inline in the CREATE TABLE, so
// Postgres named it itself — spelled out here because the name is what tells a
// re-keyed invoice number apart from any other unique violation.
const constraintCustomerInvoiceNumber = "customer_invoices_tenant_id_customer_id_invoice_number_key"

// invoiceColumns is the single source of truth for the read shape.
//
// Every SELECT in this file derives both its column list AND its scan targets
// from this slice, in this order. Three separate hand-written lists used to sit
// here, and keeping them in step was manual — the exact shape of the policy-svc
// defect, where one query drifted to 10 of 12 columns, the two it dropped stayed
// nil, serialised as null, and every affected record read as "not recorded"
// while every test passed. Nothing about that failure looks like a bug in the
// query that caused it.
var invoiceColumns = []string{
	"invoice_id",
	"tenant_id",
	"legal_entity_id",
	"customer_id",
	"invoice_number",
	"amount",
	"currency_code",
	"due_date",
	"status",
	"created_by_principal_id",
	"sent_by_principal_id",
	"marked_overdue_by_principal_id",
	"payment_received_by_principal_id",
	"correlation_id",
	"created_at",
	"sent_at",
	"marked_overdue_at",
	"payment_received_at",

	// AR-05 (migration 000006).
	"invoice_date",
	"supply_date",
	"net_amount",
	"tax_amount",
	"invoice_document_id",
	"sales_order_id",
	"customer_billing_ref",

	// AR-08 cash application.
	"payment_date",
	"payment_reference",
}

var invoiceSelectList = strings.Join(invoiceColumns, ", ")

// scanTargets returns pointers in exactly invoiceColumns' order. Reordering one
// without the other is the only way to break this pair, and init() below refuses
// to start if their lengths ever diverge.
func scanTargets(inv *domain.CustomerInvoice, status *string) []any {
	return []any{
		&inv.InvoiceID,
		&inv.TenantID,
		&inv.LegalEntityID,
		&inv.CustomerID,
		&inv.InvoiceNumber,
		&inv.Amount,
		&inv.CurrencyCode,
		&inv.DueDate,
		status,
		&inv.CreatedByPrincipalID,
		&inv.SentByPrincipalID,
		&inv.MarkedOverdueByPrincipalID,
		&inv.PaymentReceivedByPrincipalID,
		&inv.CorrelationID,
		&inv.CreatedAt,
		&inv.SentAt,
		&inv.MarkedOverdueAt,
		&inv.PaymentReceivedAt,

		&inv.InvoiceDate,
		&inv.SupplyDate,
		&inv.NetAmount,
		&inv.TaxAmount,
		&inv.InvoiceDocumentID,
		&inv.SalesOrderID,
		&inv.CustomerBillingRef,

		&inv.PaymentDate,
		&inv.PaymentReference,
	}
}

// lineColumns and lineScanTargets are the same pair for customer_invoice_lines,
// guarded by the same init() check.
var lineColumns = []string{
	"invoice_line_id",
	"invoice_id",
	"line_number",
	"description",
	"quantity",
	"unit_price",
	"net_amount",
	"tax_code",
	"tax_amount",
	"sales_order_line_ref",
	"dimensions",
}

var lineSelectList = strings.Join(lineColumns, ", ")

func lineScanTargets(l *domain.CustomerInvoiceLine) []any {
	return []any{
		&l.InvoiceLineID,
		&l.InvoiceID,
		&l.LineNumber,
		&l.Description,
		&l.Quantity,
		&l.UnitPrice,
		&l.NetAmount,
		&l.TaxCode,
		&l.TaxAmount,
		&l.SalesOrderLineRef,
		&l.Dimensions,
	}
}

func init() {
	// A count mismatch is a guaranteed runtime scan error on the first read, so
	// failing at startup is strictly better than failing per-request later.
	if n := len(scanTargets(&domain.CustomerInvoice{}, new(string))); n != len(invoiceColumns) {
		panic(fmt.Sprintf(
			"store: %d scan targets for %d invoice columns — they are derived from one list and must stay in step",
			n, len(invoiceColumns)))
	}
	if n := len(lineScanTargets(&domain.CustomerInvoiceLine{})); n != len(lineColumns) {
		panic(fmt.Sprintf(
			"store: %d scan targets for %d invoice line columns — they are derived from one list and must stay in step",
			n, len(lineColumns)))
	}
}

// mapPgError turns the driver errors that describe a CALLER's mistake into domain
// errors the handler can answer accurately. Everything else is passed through
// untouched and still reports as a store failure — a fault should stay loud.
//
// Without this, both cases below reached the handler as an opaque error and were
// answered 503 store_unavailable: a duplicate invoice number and a non-UUID
// identifier were both reported to the operator as "the database is down", which
// is wrong about where the problem is and offers nothing to do about it.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case "22P02":
		// invalid_text_representation — a non-UUID compared against a uuid
		// column. Dies inside the driver before any row is examined.
		return domain.ErrInvalidIdentifier
	case "23505":
		// unique_violation. Only the (tenant, customer, number) constraint is a
		// caller-facing duplicate. A correlation_id collision would mean the
		// ON CONFLICT clause failed to do its job, which IS a real fault and must
		// keep the loud generic error rather than being explained away as the
		// caller's mistake.
		if pgErr.ConstraintName == constraintCustomerInvoiceNumber {
			return domain.ErrDuplicateInvoiceNumber
		}
		return err
	default:
		return err
	}
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateInvoice inserts a customer invoice header in ISSUED status, and its
// lines in the same transaction.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL invoice —
// mutating *inv in place to reflect it — rather than creating a duplicate
// receivable. Returns created=false when the row already existed.
// inv.TenantID is the caller's verified scope, set by the handler from
// X-Tenant-Id — never from the request body.
func (s *PgStore) CreateInvoice(ctx context.Context, inv *domain.CustomerInvoice) (created bool, err error) {
	err = s.withRLS(ctx, inv.TenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO customer_invoices (
				invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
				amount, currency_code, due_date, status, created_by_principal_id,
				correlation_id, created_at,
				invoice_date, supply_date, net_amount, tax_amount,
				invoice_document_id, sales_order_id, customer_billing_ref
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			          $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, inv.InvoiceID, inv.TenantID, inv.LegalEntityID, inv.CustomerID, inv.InvoiceNumber,
			inv.Amount, inv.CurrencyCode, inv.DueDate, string(inv.Status), inv.CreatedByPrincipalID,
			inv.CorrelationID, now,
			inv.InvoiceDate, inv.SupplyDate, inv.NetAmount, inv.TaxAmount,
			inv.InvoiceDocumentID, inv.SalesOrderID, inv.CustomerBillingRef)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx,
				`SELECT `+invoiceSelectList+` FROM customer_invoices WHERE tenant_id = $1 AND correlation_id = $2`,
				inv.TenantID, inv.CorrelationID)
			var status string
			if err := row.Scan(scanTargets(inv, &status)...); err != nil {
				return err
			}
			inv.Status = domain.InvoiceStatus(status)

			// A replay must return the ORIGINAL invoice in full, lines included.
			// Returning the header alone would report the stored invoice with an
			// empty line list, which now reads as a pre-contract invoice — so a
			// retried POST would look like a different, older document than the
			// one it actually resolved to.
			lines, err := queryLines(ctx, tx, inv.TenantID, inv.InvoiceID)
			if err != nil {
				return err
			}
			inv.Lines = lines
			created = false
			return nil
		}

		// Lines are written in the same transaction as the header: an invoice
		// whose header committed without its lines would be a receivable with no
		// account of what it is for, and nothing later would know to add them.
		for i := range inv.Lines {
			inv.Lines[i].InvoiceLineID = uuid.NewString()
			inv.Lines[i].InvoiceID = inv.InvoiceID
			if _, err := tx.Exec(ctx, `
				INSERT INTO customer_invoice_lines (
					invoice_line_id, invoice_id, tenant_id, line_number,
					description, quantity, unit_price, net_amount,
					tax_code, tax_amount, sales_order_line_ref, dimensions
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			`, inv.Lines[i].InvoiceLineID, inv.InvoiceID, inv.TenantID, inv.Lines[i].LineNumber,
				inv.Lines[i].Description, inv.Lines[i].Quantity, inv.Lines[i].UnitPrice, inv.Lines[i].NetAmount,
				inv.Lines[i].TaxCode, inv.Lines[i].TaxAmount, inv.Lines[i].SalesOrderLineRef,
				inv.Lines[i].Dimensions); err != nil {
				return mapPgError(err)
			}
		}

		inv.CreatedAt = now
		created = true
		return nil
	})
	return created, mapPgError(err)
}

// GetInvoice returns a customer invoice by ID, scoped to tenantID — the
// caller's verified scope, which the handler has already established is
// present. Returns (nil, nil) when not found — including when the caller's
// tenant scope doesn't match the invoice's tenant (see package doc: explicit
// tenant_id filter, not RLS-only).
func (s *PgStore) GetInvoice(ctx context.Context, tenantID, invoiceID string) (*domain.CustomerInvoice, error) {
	var inv domain.CustomerInvoice
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+invoiceSelectList+` FROM customer_invoices WHERE invoice_id = $1 AND tenant_id = $2`,
			invoiceID, tenantID)
		if err := row.Scan(scanTargets(&inv, &status)...); err != nil {
			return mapPgError(err)
		}
		inv.Status = domain.InvoiceStatus(status)

		// Read in the same transaction as the header so a concurrent write
		// cannot show a header from one moment with lines from another — which
		// would make a balanced invoice read as unbalanced.
		lines, err := queryLines(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return err
		}
		inv.Lines = lines
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	// A malformed invoice_id or tenant scope cannot name an existing row, so it
	// is absent — not an outage. Reported identically to a well-formed id that
	// happens not to exist, and to another tenant's invoice, which is the whole
	// point: none of the three should be distinguishable from outside.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &inv, nil
}

// queryLines reads one invoice's lines, in line order.
//
// Scoped by tenant explicitly as well as by invoice_id, matching every other
// query in this file: RLS alone is insufficient while the service connects as a
// Postgres superuser (see migration 000003), so the predicate is what actually
// isolates tenants.
func queryLines(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]domain.CustomerInvoiceLine, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+lineSelectList+`
		 FROM customer_invoice_lines
		 WHERE invoice_id = $1 AND tenant_id = $2
		 ORDER BY line_number ASC`, invoiceID, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var lines []domain.CustomerInvoiceLine
	for rows.Next() {
		var l domain.CustomerInvoiceLine
		if err := rows.Scan(lineScanTargets(&l)...); err != nil {
			return nil, mapPgError(err)
		}
		lines = append(lines, l)
	}
	return lines, mapPgError(rows.Err())
}

// ListInvoices returns customer invoices matching the given filter.
//
// Header-only, exactly like accounts-payable-svc's register: lines belong to
// the detail read, and a page of the register is for dashboards, not
// accounting. The detail read (GetInvoice) returns them in the same transaction
// as the header.
//
// filter.TenantID is the caller's VERIFIED scope. The handler no longer lets a
// ?tenant_id= query parameter reach this far, which is what made every tenant's
// register readable by anyone: this function both filters on the value and sets
// app.tenant_id from it, so an attacker-supplied tenant satisfied the RLS
// policy it was supposed to be constrained by.
func (s *PgStore) ListInvoices(ctx context.Context, filter domain.ListInvoicesFilter) ([]domain.CustomerInvoice, error) {
	var out []domain.CustomerInvoice
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + invoiceSelectList + `
			FROM customer_invoices
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR customer_id = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC, invoice_id
			LIMIT $5 OFFSET $6
		`
		// created_at is not unique — two invoices raised in the same transaction
		// share it — so ordering by it alone leaves ties in an arbitrary order, and
		// an arbitrary order under LIMIT/OFFSET can show the same row on two pages
		// and never show another. invoice_id breaks the tie so paging is stable.
		rows, err := tx.Query(ctx, query,
			filter.TenantID, filter.LegalEntityID, filter.CustomerID, filter.Status,
			filter.Limit, filter.Offset)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var inv domain.CustomerInvoice
			var status string
			if err := rows.Scan(scanTargets(&inv, &status)...); err != nil {
				return err
			}
			inv.Status = domain.InvoiceStatus(status)
			out = append(out, inv)
		}
		return mapPgError(rows.Err())
	})
	return out, mapPgError(err)
}

// TransitionInvoice atomically moves an invoice from fromStatus to toStatus and
// returns the row as it now stands, lines included.
//
// It RETURNS the updated invoice rather than only an error. The handlers used to
// take the invoice they had read a moment earlier, set `.Status` on it by hand and
// send that back — so the response to a send, an overdue or a payment carried
// `sent_at: null` and `sent_by_principal_id: null` for the very hop it had just
// recorded. The database had the actor and the timestamp; the API denied they
// existed.
//
// paymentDate and paymentReference carry the AR-08 cash-application payload. For
// the PAID transition they are stored alongside the lifecycle stamp; for SENT
// and OVERDUE they are nil and the columns stay untouched (they are only ever
// written at the terminal transition, which nothing can follow).
func (s *PgStore) TransitionInvoice(
	ctx context.Context,
	tenantID, invoiceID string,
	fromStatus, toStatus domain.InvoiceStatus,
	actorPrincipalID string,
	paymentDate *domain.CalendarDate,
	paymentReference *string,
) (*domain.CustomerInvoice, error) {
	actorColumn, timeColumn := transitionColumns(toStatus)
	query := fmt.Sprintf(`
		UPDATE customer_invoices
		SET status = $1, %s = $2, %s = $3, payment_date = $4, payment_reference = $5
		WHERE invoice_id = $6 AND status = $7 AND tenant_id = $8
		RETURNING %s
	`, actorColumn, timeColumn, invoiceSelectList)

	var inv domain.CustomerInvoice
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, query,
			string(toStatus), actorPrincipalID, time.Now().UTC(), paymentDate, paymentReference,
			invoiceID, string(fromStatus), tenantID,
		).Scan(scanTargets(&inv, &status)...); err != nil {
			return err
		}
		inv.Status = domain.InvoiceStatus(status)

		// The response reports the whole invoice as it now stands, lines
		// included — the same contract the create and detail reads keep.
		lines, err := queryLines(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return err
		}
		inv.Lines = lines
		return nil
	})
	// No rows means the WHERE did not match: wrong status, wrong tenant, or no such
	// invoice. All three are "you cannot make that move from here", which is what
	// the conditional UPDATE exists to decide atomically.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &inv, nil
}

func transitionColumns(to domain.InvoiceStatus) (actorColumn, timeColumn string) {
	switch to {
	case domain.InvoiceStatusSent:
		return "sent_by_principal_id", "sent_at"
	case domain.InvoiceStatusOverdue:
		return "marked_overdue_by_principal_id", "marked_overdue_at"
	case domain.InvoiceStatusPaid:
		return "payment_received_by_principal_id", "payment_received_at"
	default:
		return "sent_by_principal_id", "sent_at"
	}
}