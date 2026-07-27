package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/compensation-svc/internal/domain"
	svcmiddleware "zoiko.io/compensation-svc/internal/middleware"
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

func (s *PgStore) CreateStructure(ctx context.Context, str *domain.CompensationStructure) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO compensation_structures (
				structure_id, tenant_id, legal_entity_id, name, pay_type,
				min_amount, max_amount, currency, overtime_multiplier, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, str.StructureID, tenantID, str.LegalEntityID, str.Name, str.PayType,
			str.MinAmount, str.MaxAmount, str.Currency, str.OvertimeMultiplier, str.CreatedAt, str.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListStructures(ctx context.Context, legalEntityID string) ([]domain.CompensationStructure, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.CompensationStructure
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT structure_id, tenant_id, legal_entity_id, name, pay_type,
			       min_amount, max_amount, currency, overtime_multiplier, created_at, updated_at
			FROM compensation_structures
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += " AND legal_entity_id = $2"
		}
		query += " ORDER BY name ASC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var str domain.CompensationStructure
			if err := rows.Scan(
				&str.StructureID, &str.TenantID, &str.LegalEntityID, &str.Name, &str.PayType,
				&str.MinAmount, &str.MaxAmount, &str.Currency, &str.OvertimeMultiplier, &str.CreatedAt, &str.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, str)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) CreateWageRevision(ctx context.Context, rev *domain.WageRevision) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Update active wage revision for employee (set status SUPERSEDED and effective_to)
		_, err := tx.Exec(ctx, `
			UPDATE wage_revisions
			SET status = 'SUPERSEDED', effective_to = $1
			WHERE tenant_id = $2 AND employee_id = $3 AND status = 'ACTIVE'
		`, rev.EffectiveFrom, tenantID, rev.EmployeeID)
		if err != nil {
			return err
		}

		// 2. Insert new active wage revision
		_, err = tx.Exec(ctx, `
			INSERT INTO wage_revisions (
				revision_id, tenant_id, employee_id, structure_id, pay_type,
				amount, currency, effective_from, effective_to, reason, revised_by, status, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, rev.RevisionID, tenantID, rev.EmployeeID, rev.StructureID, rev.PayType,
			rev.Amount, rev.Currency, rev.EffectiveFrom, rev.EffectiveTo, rev.Reason, rev.RevisedBy, rev.Status, rev.CreatedAt)
		return err
	})
}

func (s *PgStore) GetActiveWageRevision(ctx context.Context, employeeID string) (*domain.WageRevision, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var rev domain.WageRevision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT revision_id, tenant_id, employee_id, structure_id, pay_type,
			       amount, currency, effective_from, effective_to, reason, revised_by, status, created_at
			FROM wage_revisions
			WHERE tenant_id = $1 AND employee_id = $2 AND status = 'ACTIVE'
			LIMIT 1
		`, tenantID, employeeID).Scan(
			&rev.RevisionID, &rev.TenantID, &rev.EmployeeID, &rev.StructureID, &rev.PayType,
			&rev.Amount, &rev.Currency, &rev.EffectiveFrom, &rev.EffectiveTo, &rev.Reason, &rev.RevisedBy, &rev.Status, &rev.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWageRevisionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

func (s *PgStore) GetWageRevisionHistory(ctx context.Context, employeeID string) ([]domain.WageRevision, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.WageRevision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT revision_id, tenant_id, employee_id, structure_id, pay_type,
			       amount, currency, effective_from, effective_to, reason, revised_by, status, created_at
			FROM wage_revisions
			WHERE tenant_id = $1 AND employee_id = $2
			ORDER BY effective_from DESC, created_at DESC
		`, tenantID, employeeID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var rev domain.WageRevision
			if err := rows.Scan(
				&rev.RevisionID, &rev.TenantID, &rev.EmployeeID, &rev.StructureID, &rev.PayType,
				&rev.Amount, &rev.Currency, &rev.EffectiveFrom, &rev.EffectiveTo, &rev.Reason, &rev.RevisedBy, &rev.Status, &rev.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, rev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) CreateBonusGrant(ctx context.Context, b *domain.BonusGrant) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO bonus_grants (
				grant_id, tenant_id, employee_id, bonus_type, amount,
				currency, grant_date, status, approved_by, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, b.GrantID, tenantID, b.EmployeeID, b.BonusType, b.Amount,
			b.Currency, b.GrantDate, b.Status, b.ApprovedBy, b.CreatedAt)
		return err
	})
}

func (s *PgStore) ApproveBonusGrant(ctx context.Context, grantID, approvedBy string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM bonus_grants WHERE grant_id = $1 AND tenant_id = $2 FOR UPDATE", grantID, tenantID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrBonusNotFound
		}
		if err != nil {
			return err
		}

		if status != "PENDING" {
			return domain.ErrInvalidBonusStatus
		}

		_, err = tx.Exec(ctx, `
			UPDATE bonus_grants
			SET status = 'APPROVED', approved_by = $1
			WHERE grant_id = $2 AND tenant_id = $3
		`, approvedBy, grantID, tenantID)
		return err
	})
}

func (s *PgStore) ListBonusGrants(ctx context.Context, employeeID, status string) ([]domain.BonusGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BonusGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT grant_id, tenant_id, employee_id, bonus_type, amount,
			       currency, grant_date, status, approved_by, created_at
			FROM bonus_grants
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if employeeID != "" {
			args = append(args, employeeID)
			query += fmt.Sprintf(" AND employee_id = $%d", len(args))
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
			var b domain.BonusGrant
			if err := rows.Scan(
				&b.GrantID, &b.TenantID, &b.EmployeeID, &b.BonusType, &b.Amount,
				&b.Currency, &b.GrantDate, &b.Status, &b.ApprovedBy, &b.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}