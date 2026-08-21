// Package store provides the PostgreSQL implementation of accounts-payable-svc's
// persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policies in
// deployments/migrations/000001_initial_schema.up.sql are real and correctly
// written. But every method ALSO filters explicitly by tenant_id in its own
// SQL, rather than relying on RLS alone: this pool connects as a Postgres
// superuser (DB_USER=postgres, same as every other service in this
// platform), and Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy. Found via a genuine CI failure in
// general-ledger-svc (TestPgStore_RLS_TenantIsolation caught real
// cross-tenant leakage there), so this service is built with the explicit
// filter from day one rather than discovering the same gap a second time.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
)

// invoiceColumns is the single source of truth for the read shape.
//
// Every SELECT in this file derives both its column list AND its scan targets
// from this slice, in this order. Three separate hand-written lists used to sit
// here — two of 18 columns and one of 16 — and keeping them in step was manual.
// That is the exact shape of the policy-svc defect, where one query had drifted
// to 10 of 12 columns: the two it dropped stayed nil, serialised as null, and
// every affected record read as "not recorded" while every test passed. Nothing
// about that failure looks like a bug in the query that caused it.
var invoiceColumns = []string{
	"invoice_id",
	"tenant_id",
	"legal_entity_id",
	"vendor_id",
	"invoice_number",
	"amount",
	"currency_code",
	"due_date",
	"status",
	"source_contract_id",
	"created_by_principal_id",
	"validated_by_principal_id",
	"approved_by_principal_id",
	"payment_requested_by_principal_id",
	"correlation_id",
	"created_at",
	"validated_at",
	"approved_at",
	"payment_requested_at",
}

var invoiceSelectList = strings.Join(invoiceColumns, ", ")

// scanTargets returns pointers in exactly invoiceColumns' order. Reordering one
// without the other is the only way to break this pair, and init() below refuses
// to start if their lengths ever diverge.
func scanTargets(inv *domain.VendorInvoice, status *string) []any {
	return []any{
		&inv.InvoiceID,
		&inv.TenantID,
		&inv.LegalEntityID,
		&inv.VendorID,
		&inv.InvoiceNumber,
		&inv.Amount,
		&inv.CurrencyCode,
		&inv.DueDate,
		status,
		&inv.SourceContractID,
		&inv.CreatedByPrincipalID,
		&inv.ValidatedByPrincipalID,
		&inv.ApprovedByPrincipalID,
		&inv.PaymentRequestedByPrincipalID,
		&inv.CorrelationID,
		&inv.CreatedAt,
		&inv.ValidatedAt,
		&inv.ApprovedAt,
		&inv.PaymentRequestedAt,
	}
}

func init() {
	// A count mismatch is a guaranteed runtime scan error on the first read, so
	// failing at startup is strictly better than failing per-request later.
	if n := len(scanTargets(&domain.VendorInvoice{}, new(string))); n != len(invoiceColumns) {
		panic(fmt.Sprintf(
			"store: %d scan targets for %d invoice columns — they are derived from one list and must stay in step",
			n, len(invoiceColumns)))
	}
}

// Constraint names from the migrations. Matched by name rather than by column
// guesswork so a future unique constraint cannot be silently reported as this
// one.
const (
	constraintVendorInvoiceNumber = "vendor_invoices_tenant_id_vendor_id_invoice_number_key"
	indexTenantCorrelation        = "idx_vendor_invoices_tenant_correlation"
)

// mapPgError translates the Postgres failures that are really caller mistakes
// into domain errors, so they stop arriving as "the store is unavailable".
//
// Both of these previously reached the handler as generic errors and answered
// 503 — a status that tells an operator to go and look at infrastructure. A
// mistyped id in a URL and a re-keyed invoice number are neither of them an
// outage, and reporting them as one is worse than a plain 500: it is confidently
// wrong about where the problem is.
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
		// unique_violation. Only the (tenant, vendor, number) constraint is a
		// caller-facing duplicate; a correlation_id collision means the
		// ON CONFLICT clause failed to do its job, which IS a real fault and
		// must keep its loud generic error rather than being explained away.
		if pgErr.ConstraintName == constraintVendorInvoiceNumber {
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

// tenantFromCtxOrFallback used to live here, resolving the RLS scope from the
// context but FALLING BACK to whatever the caller had supplied in the request
// body. A request carrying no X-Tenant-Id therefore chose its own scope: the
// body's tenant_id was handed to set_config('app.tenant_id') AND written into
// the row, so the policy that should have refused the insert was satisfied by
// the value under attack. The handler now resolves the tenant once from the
// verified header; there is deliberately no fallback left to reach for.

// CreateInvoice inserts a vendor invoice header in RECEIVED status.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL invoice —
// mutating *inv in place to reflect it — rather than creating a duplicate
// liability. Returns created=false when the row already existed.
func (s *PgStore) CreateInvoice(ctx context.Context, inv *domain.VendorInvoice) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrTenantScopeMissing
	}
	// The row is filed under the verified scope, not under inv.TenantID — which
	// is what the INSERT used to write while app.tenant_id was set from the same
	// unverified value.
	inv.TenantID = tenantID

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO vendor_invoices (
				invoice_id, tenant_id, legal_entity_id, vendor_id, invoice_number,
				amount, currency_code, due_date, status, source_contract_id, created_by_principal_id,
				correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, inv.InvoiceID, inv.TenantID, inv.LegalEntityID, inv.VendorID, inv.InvoiceNumber,
			inv.Amount, inv.CurrencyCode, inv.DueDate, string(inv.Status), inv.SourceContractID, inv.CreatedByPrincipalID,
			inv.CorrelationID, now)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx,
				`SELECT `+invoiceSelectList+` FROM vendor_invoices WHERE tenant_id = $1 AND correlation_id = $2`,
				inv.TenantID, inv.CorrelationID)
			var status string
			if err := row.Scan(scanTargets(inv, &status)...); err != nil {
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
	return created, err
}

// GetInvoice returns a vendor invoice by ID, scoped to the caller's tenant.
// Returns (nil, nil) if not found — including when the caller's tenant scope
// doesn't match the invoice's tenant (see package doc: explicit tenant_id
// filter, not RLS-only).
func (s *PgStore) GetInvoice(ctx context.Context, invoiceID string) (*domain.VendorInvoice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var inv domain.VendorInvoice
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+invoiceSelectList+` FROM vendor_invoices WHERE invoice_id = $1 AND tenant_id = $2`,
			invoiceID, tenantID)
		if err := row.Scan(scanTargets(&inv, &status)...); err != nil {
			return mapPgError(err)
		}
		inv.Status = domain.InvoiceStatus(status)
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
		return nil, err
	}
	return &inv, nil
}

// ListInvoices returns vendor invoices matching the given filter (tenant_id
// is required; the others are optional).
func (s *PgStore) ListInvoices(ctx context.Context, filter domain.ListInvoicesFilter) ([]domain.VendorInvoice, error) {
	var out []domain.VendorInvoice
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + invoiceSelectList + `
			FROM vendor_invoices
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR vendor_id = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC
		`
		rows, err := tx.Query(ctx, query, filter.TenantID, filter.LegalEntityID, filter.VendorID, filter.Status)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var inv domain.VendorInvoice
			var status string
			if err := rows.Scan(scanTargets(&inv, &status)...); err != nil {
				return err
			}
			inv.Status = domain.InvoiceStatus(status)
			out = append(out, inv)
		}
		return mapPgError(rows.Err())
	})
	return out, err
}

// TransitionInvoice atomically moves an invoice from fromStatus to toStatus,
// stamping the actor and timestamp column appropriate to toStatus. Uses
// WHERE status = $fromStatus AND tenant_id = $tenantID so the transition,
// the state-machine check, and the tenant scope are one atomic UPDATE — no
// separate read, no race window (same pattern as general-ledger-svc's
// TransitionJournal). Returns domain.ErrInvalidTransition if zero rows were
// affected (the invoice doesn't exist, wasn't in fromStatus, or belongs to a
// different tenant).
func (s *PgStore) TransitionInvoice(ctx context.Context, tenantID, invoiceID string, fromStatus, toStatus domain.InvoiceStatus, actorPrincipalID string) error {
	actorColumn, timeColumn := transitionColumns(toStatus)
	query := fmt.Sprintf(`
		UPDATE vendor_invoices
		SET status = $1, %s = $2, %s = $3
		WHERE invoice_id = $4 AND status = $5 AND tenant_id = $6
	`, actorColumn, timeColumn)

	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, string(toStatus), actorPrincipalID, time.Now().UTC(), invoiceID, string(fromStatus), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		affected = tag.RowsAffected()
		return nil
	})
	// A malformed id names no row, so this is the same answer as an unknown one:
	// not found, which the handler turns into 404 rather than 503.
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return domain.ErrInvoiceNotFound
	}
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

func transitionColumns(to domain.InvoiceStatus) (actorColumn, timeColumn string) {
	switch to {
	case domain.InvoiceStatusValidated:
		return "validated_by_principal_id", "validated_at"
	case domain.InvoiceStatusApproved:
		return "approved_by_principal_id", "approved_at"
	case domain.InvoiceStatusPaymentRequested:
		return "payment_requested_by_principal_id", "payment_requested_at"
	default:
		return "approved_by_principal_id", "approved_at"
	}
}
