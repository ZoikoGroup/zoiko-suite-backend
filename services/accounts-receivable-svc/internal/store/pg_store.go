package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tenantFromCtxOrFallback used to live here, resolving the RLS scope from the
// context and falling back to whatever the request body or query string had
// said when the context carried none. That fallback WAS the vulnerability: a
// request with no X-Tenant-Id chose its own scope, and the RLS session variable
// was then set from the attacker's value so the policy waved it through. The
// tenant is now resolved once in the handler (requireTenant) and passed in, so
// there is no second source for it to disagree with.

// CreateInvoice inserts a customer invoice header in ISSUED status.
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
				correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, inv.InvoiceID, inv.TenantID, inv.LegalEntityID, inv.CustomerID, inv.InvoiceNumber,
			inv.Amount, inv.CurrencyCode, inv.DueDate, string(inv.Status), inv.CreatedByPrincipalID,
			inv.CorrelationID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT invoice_id, legal_entity_id, customer_id, invoice_number, amount, currency_code,
				       due_date, status, created_by_principal_id, sent_by_principal_id,
				       marked_overdue_by_principal_id, payment_received_by_principal_id,
				       created_at, sent_at, marked_overdue_at, payment_received_at
				FROM customer_invoices WHERE tenant_id = $1 AND correlation_id = $2
			`, inv.TenantID, inv.CorrelationID)
			var status string
			if err := row.Scan(
				&inv.InvoiceID, &inv.LegalEntityID, &inv.CustomerID, &inv.InvoiceNumber, &inv.Amount, &inv.CurrencyCode,
				&inv.DueDate, &status, &inv.CreatedByPrincipalID, &inv.SentByPrincipalID,
				&inv.MarkedOverdueByPrincipalID, &inv.PaymentReceivedByPrincipalID,
				&inv.CreatedAt, &inv.SentAt, &inv.MarkedOverdueAt, &inv.PaymentReceivedAt,
			); err != nil {
				return err
			}
			inv.Status = domain.InvoiceStatus(status)
			created = false
			return nil
		}
		inv.CreatedAt = now
		created = true
		return nil
	})
	return created, mapPgError(err)
}

// GetInvoice returns a customer invoice by ID, scoped to tenantID — the
// caller's verified scope, which the handler has already established is
// present. It used to read the tenant from the context itself and answer
// (nil, nil) when there was none, which the handler reported as
// invoice_not_found: a request missing its identity header was told the invoice
// did not exist.
func (s *PgStore) GetInvoice(ctx context.Context, tenantID, invoiceID string) (*domain.CustomerInvoice, error) {
	var inv domain.CustomerInvoice
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
			       amount, currency_code, due_date, status, created_by_principal_id,
			       sent_by_principal_id, marked_overdue_by_principal_id, payment_received_by_principal_id,
			       correlation_id, created_at, sent_at, marked_overdue_at, payment_received_at
			FROM customer_invoices WHERE invoice_id = $1 AND tenant_id = $2
		`, invoiceID, tenantID)
		if err := row.Scan(
			&inv.InvoiceID, &inv.TenantID, &inv.LegalEntityID, &inv.CustomerID, &inv.InvoiceNumber,
			&inv.Amount, &inv.CurrencyCode, &inv.DueDate, &status, &inv.CreatedByPrincipalID,
			&inv.SentByPrincipalID, &inv.MarkedOverdueByPrincipalID, &inv.PaymentReceivedByPrincipalID,
			&inv.CorrelationID, &inv.CreatedAt, &inv.SentAt, &inv.MarkedOverdueAt, &inv.PaymentReceivedAt,
		); err != nil {
			return err
		}
		inv.Status = domain.InvoiceStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &inv, nil
}

// ListInvoices returns customer invoices matching the given filter.
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
			SELECT invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
			       amount, currency_code, due_date, status, created_by_principal_id,
			       sent_by_principal_id, marked_overdue_by_principal_id, payment_received_by_principal_id,
			       correlation_id, created_at, sent_at, marked_overdue_at, payment_received_at
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
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var inv domain.CustomerInvoice
			var status string
			if err := rows.Scan(
				&inv.InvoiceID, &inv.TenantID, &inv.LegalEntityID, &inv.CustomerID, &inv.InvoiceNumber,
				&inv.Amount, &inv.CurrencyCode, &inv.DueDate, &status, &inv.CreatedByPrincipalID,
				&inv.SentByPrincipalID, &inv.MarkedOverdueByPrincipalID, &inv.PaymentReceivedByPrincipalID,
				&inv.CorrelationID, &inv.CreatedAt, &inv.SentAt, &inv.MarkedOverdueAt, &inv.PaymentReceivedAt,
			); err != nil {
				return err
			}
			inv.Status = domain.InvoiceStatus(status)
			out = append(out, inv)
		}
		return rows.Err()
	})
	return out, mapPgError(err)
}

// TransitionInvoice atomically moves an invoice from fromStatus to toStatus and
// returns the row as it now stands.
//
// It RETURNS the updated invoice rather than only an error. The handlers used to
// take the invoice they had read a moment earlier, set `.Status` on it by hand and
// send that back — so the response to a send, an overdue or a payment carried
// `sent_at: null` and `sent_by_principal_id: null` for the very hop it had just
// recorded. The database had the actor and the timestamp; the API denied they
// existed. Reading them back from the UPDATE is also the only way to report them
// without a second query that could see a different row.
func (s *PgStore) TransitionInvoice(
	ctx context.Context,
	tenantID, invoiceID string,
	fromStatus, toStatus domain.InvoiceStatus,
	actorPrincipalID string,
) (*domain.CustomerInvoice, error) {
	actorColumn, timeColumn := transitionColumns(toStatus)
	query := fmt.Sprintf(`
		UPDATE customer_invoices
		SET status = $1, %s = $2, %s = $3
		WHERE invoice_id = $4 AND status = $5 AND tenant_id = $6
		RETURNING invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
		          amount, currency_code, due_date, status, created_by_principal_id,
		          sent_by_principal_id, marked_overdue_by_principal_id, payment_received_by_principal_id,
		          correlation_id, created_at, sent_at, marked_overdue_at, payment_received_at
	`, actorColumn, timeColumn)

	var inv domain.CustomerInvoice
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query,
			string(toStatus), actorPrincipalID, time.Now().UTC(), invoiceID, string(fromStatus), tenantID,
		).Scan(
			&inv.InvoiceID, &inv.TenantID, &inv.LegalEntityID, &inv.CustomerID, &inv.InvoiceNumber,
			&inv.Amount, &inv.CurrencyCode, &inv.DueDate, &status, &inv.CreatedByPrincipalID,
			&inv.SentByPrincipalID, &inv.MarkedOverdueByPrincipalID, &inv.PaymentReceivedByPrincipalID,
			&inv.CorrelationID, &inv.CreatedAt, &inv.SentAt, &inv.MarkedOverdueAt, &inv.PaymentReceivedAt,
		)
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
	inv.Status = domain.InvoiceStatus(status)
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
