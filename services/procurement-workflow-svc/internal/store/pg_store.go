package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/procurement-workflow-svc/internal/domain"
	svcmiddleware "zoiko.io/procurement-workflow-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const caseColumns = `
	case_id, tenant_id, legal_entity_id, requested_by_principal_id, description,
	category, amount, currency_code, vendor_profile_id, status,
	COALESCE(spend_check_decision, ''), COALESCE(spend_check_basis, ''), purchase_order_id,
	approved_by_principal_id, rejected_by_principal_id, rejection_reason,
	correlation_id, created_at, updated_at, approved_at, rejected_at, completed_at
`

func scanCase(row pgx.Row, c *domain.ProcurementCase) error {
	var status string
	if err := row.Scan(
		&c.CaseID, &c.TenantID, &c.LegalEntityID, &c.RequestedByPrincipalID, &c.Description,
		&c.Category, &c.Amount, &c.CurrencyCode, &c.VendorProfileID, &status,
		&c.SpendCheckDecision, &c.SpendCheckBasis, &c.PurchaseOrderID,
		&c.ApprovedByPrincipalID, &c.RejectedByPrincipalID, &c.RejectionReason,
		&c.CorrelationID, &c.CreatedAt, &c.UpdatedAt, &c.ApprovedAt, &c.RejectedAt, &c.CompletedAt,
	); err != nil {
		return err
	}
	c.Status = domain.CaseStatus(status)
	return nil
}

// CreateCase inserts a new procurement case, idempotent on
// (tenant_id, correlation_id): if a retry races a concurrent original
// request, the loser fetches and returns the winner's row instead of
// erroring or creating a duplicate case.
func (s *PgStore) CreateCase(ctx context.Context, c *domain.ProcurementCase) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO procurement_cases (
				case_id, tenant_id, legal_entity_id, requested_by_principal_id, description,
				category, amount, currency_code, vendor_profile_id, status,
				spend_check_decision, spend_check_basis, purchase_order_id,
				approved_by_principal_id, rejected_by_principal_id, rejection_reason,
				correlation_id, created_at, updated_at, approved_at, rejected_at, completed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, c.CaseID, tenantID, c.LegalEntityID, c.RequestedByPrincipalID, c.Description,
			c.Category, c.Amount, c.CurrencyCode, c.VendorProfileID, string(c.Status),
			c.SpendCheckDecision, c.SpendCheckBasis, c.PurchaseOrderID,
			c.ApprovedByPrincipalID, c.RejectedByPrincipalID, c.RejectionReason,
			c.CorrelationID, c.CreatedAt, c.UpdatedAt, c.ApprovedAt, c.RejectedAt, c.CompletedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}

		row := tx.QueryRow(ctx, "SELECT "+caseColumns+" FROM procurement_cases WHERE tenant_id = $1 AND correlation_id = $2", tenantID, c.CorrelationID)
		return scanCase(row, c)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetCase(ctx context.Context, caseID string) (*domain.ProcurementCase, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.ProcurementCase
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+caseColumns+" FROM procurement_cases WHERE tenant_id = $1 AND case_id = $2", tenantID, caseID)
		return scanCase(row, &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) ListCases(ctx context.Context, legalEntityID, status string) ([]domain.ProcurementCase, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ProcurementCase
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + caseColumns + " FROM procurement_cases WHERE tenant_id = $1"
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.ProcurementCase
			if err := scanCase(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateApproved transitions a case from APPROVAL_PENDING to APPROVED,
// re-checking the current status inside the same transaction the update
// runs in so a concurrent double-approve cannot race past the guard.
func (s *PgStore) UpdateApproved(ctx context.Context, caseID, principalID string) (*domain.ProcurementCase, error) {
	return s.transition(ctx, caseID, domain.CaseStatusApprovalPending, func(tx pgx.Tx, tenantID string, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE procurement_cases
			SET status = $1, approved_by_principal_id = $2, approved_at = $3, updated_at = $3
			WHERE tenant_id = $4 AND case_id = $5
		`, string(domain.CaseStatusApproved), principalID, now, tenantID, caseID)
		return err
	})
}

// UpdateRejected transitions a case from APPROVAL_PENDING to REJECTED.
func (s *PgStore) UpdateRejected(ctx context.Context, caseID, principalID, reason string) (*domain.ProcurementCase, error) {
	return s.transition(ctx, caseID, domain.CaseStatusApprovalPending, func(tx pgx.Tx, tenantID string, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE procurement_cases
			SET status = $1, rejected_by_principal_id = $2, rejection_reason = $3, rejected_at = $4, updated_at = $4
			WHERE tenant_id = $5 AND case_id = $6
		`, string(domain.CaseStatusRejected), principalID, reason, now, tenantID, caseID)
		return err
	})
}

// UpdateOrderIssued transitions a case from APPROVED to COMPLETED, recording
// the purchase order that was issued for it.
func (s *PgStore) UpdateOrderIssued(ctx context.Context, caseID, purchaseOrderID string) (*domain.ProcurementCase, error) {
	return s.transition(ctx, caseID, domain.CaseStatusApproved, func(tx pgx.Tx, tenantID string, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE procurement_cases
			SET status = $1, purchase_order_id = $2, completed_at = $3, updated_at = $3
			WHERE tenant_id = $4 AND case_id = $5
		`, string(domain.CaseStatusCompleted), purchaseOrderID, now, tenantID, caseID)
		return err
	})
}

// transition fetches the case, verifies it is currently in requiredStatus,
// runs the caller's UPDATE, and re-fetches the final row — all inside one
// RLS-scoped transaction so the check-then-act is atomic.
func (s *PgStore) transition(ctx context.Context, caseID string, requiredStatus domain.CaseStatus, update func(tx pgx.Tx, tenantID string, now time.Time) error) (*domain.ProcurementCase, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out domain.ProcurementCase
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var current domain.ProcurementCase
		row := tx.QueryRow(ctx, "SELECT "+caseColumns+" FROM procurement_cases WHERE tenant_id = $1 AND case_id = $2 FOR UPDATE", tenantID, caseID)
		if err := scanCase(row, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			return err
		}
		if current.Status != requiredStatus {
			return domain.ErrInvalidTransition
		}

		if err := update(tx, tenantID, time.Now().UTC()); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, "SELECT "+caseColumns+" FROM procurement_cases WHERE tenant_id = $1 AND case_id = $2", tenantID, caseID)
		return scanCase(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
