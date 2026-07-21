package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
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
	defer tx.Rollback(ctx)

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

// CreateInvoice inserts a customer invoice header in ISSUED status.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL invoice —
// mutating *inv in place to reflect it — rather than creating a duplicate
// receivable. Returns created=false when the row already existed.
func (s *PgStore) CreateInvoice(ctx context.Context, inv *domain.CustomerInvoice) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, inv.TenantID)

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
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
	return created, err
}

// GetInvoice returns a customer invoice by ID, scoped to the caller's tenant.
// Returns (nil, nil) if not found.
func (s *PgStore) GetInvoice(ctx context.Context, invoiceID string) (*domain.CustomerInvoice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

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
		return nil, err
	}
	return &inv, nil
}

// ListInvoices returns customer invoices matching the given filter.
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
			ORDER BY created_at DESC
		`
		rows, err := tx.Query(ctx, query, filter.TenantID, filter.LegalEntityID, filter.CustomerID, filter.Status)
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
	return out, err
}

// TransitionInvoice atomically moves an invoice from fromStatus to toStatus.
func (s *PgStore) TransitionInvoice(ctx context.Context, tenantID, invoiceID string, fromStatus, toStatus domain.InvoiceStatus, actorPrincipalID string) error {
	actorColumn, timeColumn := transitionColumns(toStatus)
	query := fmt.Sprintf(`
		UPDATE customer_invoices
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
