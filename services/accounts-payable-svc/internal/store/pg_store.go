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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
)

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

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// CreateInvoice inserts a vendor invoice header in RECEIVED status.
func (s *PgStore) CreateInvoice(ctx context.Context, inv *domain.VendorInvoice) error {
	tenantID := tenantFromCtxOrFallback(ctx, inv.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, `
			INSERT INTO vendor_invoices (
				invoice_id, tenant_id, legal_entity_id, vendor_id, invoice_number,
				amount, currency_code, due_date, status, created_by_principal_id,
				correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, inv.InvoiceID, inv.TenantID, inv.LegalEntityID, inv.VendorID, inv.InvoiceNumber,
			inv.Amount, inv.CurrencyCode, inv.DueDate, string(inv.Status), inv.CreatedByPrincipalID,
			inv.CorrelationID, now)
		if err != nil {
			return err
		}
		inv.CreatedAt = now
		return nil
	})
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
		row := tx.QueryRow(ctx, `
			SELECT invoice_id, tenant_id, legal_entity_id, vendor_id, invoice_number,
			       amount, currency_code, due_date, status, created_by_principal_id,
			       validated_by_principal_id, approved_by_principal_id, payment_requested_by_principal_id,
			       correlation_id, created_at, validated_at, approved_at, payment_requested_at
			FROM vendor_invoices WHERE invoice_id = $1 AND tenant_id = $2
		`, invoiceID, tenantID)
		if err := row.Scan(
			&inv.InvoiceID, &inv.TenantID, &inv.LegalEntityID, &inv.VendorID, &inv.InvoiceNumber,
			&inv.Amount, &inv.CurrencyCode, &inv.DueDate, &status, &inv.CreatedByPrincipalID,
			&inv.ValidatedByPrincipalID, &inv.ApprovedByPrincipalID, &inv.PaymentRequestedByPrincipalID,
			&inv.CorrelationID, &inv.CreatedAt, &inv.ValidatedAt, &inv.ApprovedAt, &inv.PaymentRequestedAt,
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
			SELECT invoice_id, tenant_id, legal_entity_id, vendor_id, invoice_number,
			       amount, currency_code, due_date, status, created_by_principal_id,
			       validated_by_principal_id, approved_by_principal_id, payment_requested_by_principal_id,
			       correlation_id, created_at, validated_at, approved_at, payment_requested_at
			FROM vendor_invoices
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR vendor_id = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC
		`
		rows, err := tx.Query(ctx, query, filter.TenantID, filter.LegalEntityID, filter.VendorID, filter.Status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var inv domain.VendorInvoice
			var status string
			if err := rows.Scan(
				&inv.InvoiceID, &inv.TenantID, &inv.LegalEntityID, &inv.VendorID, &inv.InvoiceNumber,
				&inv.Amount, &inv.CurrencyCode, &inv.DueDate, &status, &inv.CreatedByPrincipalID,
				&inv.ValidatedByPrincipalID, &inv.ApprovedByPrincipalID, &inv.PaymentRequestedByPrincipalID,
				&inv.CorrelationID, &inv.CreatedAt, &inv.ValidatedAt, &inv.ApprovedAt, &inv.PaymentRequestedAt,
			); err != nil {
				return err
			}
			inv.Status = domain.InvoiceStatus(status)
			out = append(out, inv)
		}
		return rows.Err()
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
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
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
