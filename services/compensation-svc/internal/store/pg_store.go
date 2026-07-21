package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/compensation-svc/internal/domain"
	svcmiddleware "zoiko.io/compensation-svc/internal/middleware"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

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

// CreateStructure inserts a compensation structure.
//
// Idempotent on (tenant_id, correlation_id): a retried call resolves to
// the ORIGINAL structure — mutating *str in place to reflect it — rather
// than creating a duplicate.
func (s *PgStore) CreateStructure(ctx context.Context, str *domain.CompensationStructure) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO compensation_structures (
				structure_id, tenant_id, legal_entity_id, name, pay_type,
				min_amount, max_amount, currency, overtime_multiplier, correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, str.StructureID, tenantID, str.LegalEntityID, str.Name, str.PayType,
			str.MinAmount, str.MaxAmount, str.Currency, str.OvertimeMultiplier, str.CorrelationID, str.CreatedAt, str.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT structure_id, legal_entity_id, name, pay_type, min_amount, max_amount,
				       currency, overtime_multiplier, created_at, updated_at
				FROM compensation_structures WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, str.CorrelationID)
			if err := row.Scan(
				&str.StructureID, &str.LegalEntityID, &str.Name, &str.PayType, &str.MinAmount, &str.MaxAmount,
				&str.Currency, &str.OvertimeMultiplier, &str.CreatedAt, &str.UpdatedAt,
			); err != nil {
				return err
			}
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
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

// CreateWageRevision supersedes the employee's current active revision
// (if any) and inserts a new one.
//
// Idempotent on (tenant_id, correlation_id): a retried call resolves to
// the ORIGINAL revision — mutating *rev in place — rather than superseding
// an already-superseded chain a second time.
//
// Race safety: a unique partial index on (tenant_id, employee_id) WHERE
// status = 'ACTIVE' (migration 000002) makes the final INSERT fail with a
// constraint violation if a concurrent call already committed a new
// ACTIVE row for this employee between this transaction's supersede step
// and its insert — surfaced as domain.ErrConcurrentWageRevision rather
// than silently leaving two ACTIVE rows for one employee.
func (s *PgStore) CreateWageRevision(ctx context.Context, rev *domain.WageRevision) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if rev.CorrelationID != "" {
			var existingID string
			err := tx.QueryRow(ctx, `SELECT revision_id FROM wage_revisions WHERE tenant_id = $1 AND correlation_id = $2`,
				tenantID, rev.CorrelationID).Scan(&existingID)
			if err == nil {
				row := tx.QueryRow(ctx, `
					SELECT revision_id, employee_id, structure_id, pay_type, amount, currency,
					       effective_from::text, effective_to::text, reason, revised_by, status, created_at
					FROM wage_revisions WHERE revision_id = $1 AND tenant_id = $2
				`, existingID, tenantID)
				if err := row.Scan(
					&rev.RevisionID, &rev.EmployeeID, &rev.StructureID, &rev.PayType, &rev.Amount, &rev.Currency,
					&rev.EffectiveFrom, &rev.EffectiveTo, &rev.Reason, &rev.RevisedBy, &rev.Status, &rev.CreatedAt,
				); err != nil {
					return err
				}
				created = false
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		// 1. Supersede the employee's current active revision, if any.
		_, err := tx.Exec(ctx, `
			UPDATE wage_revisions
			SET status = 'SUPERSEDED', effective_to = $1
			WHERE tenant_id = $2 AND employee_id = $3 AND status = 'ACTIVE'
		`, rev.EffectiveFrom, tenantID, rev.EmployeeID)
		if err != nil {
			return err
		}

		// 2. Insert new active wage revision. A concurrent call that
		// superseded the same row and is racing to insert its own new
		// ACTIVE row will have exactly one of the two INSERTs rejected by
		// idx_wage_revisions_one_active.
		_, err = tx.Exec(ctx, `
			INSERT INTO wage_revisions (
				revision_id, tenant_id, employee_id, structure_id, pay_type,
				amount, currency, effective_from, effective_to, reason, revised_by, status, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`, rev.RevisionID, tenantID, rev.EmployeeID, rev.StructureID, rev.PayType,
			rev.Amount, rev.Currency, rev.EffectiveFrom, rev.EffectiveTo, rev.Reason, rev.RevisedBy, rev.Status, rev.CorrelationID, rev.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrConcurrentWageRevision
			}
			return err
		}
		created = true
		return nil
	})
	return created, err
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
			       amount, currency, effective_from::text, effective_to::text, reason, revised_by, status, created_at
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
			       amount, currency, effective_from::text, effective_to::text, reason, revised_by, status, created_at
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

// CreateBonusGrant inserts a bonus grant in PENDING status.
//
// Idempotent on (tenant_id, correlation_id): a retried call resolves to
// the ORIGINAL grant — mutating *b in place — rather than creating a
// second, independently-approvable duplicate bonus payout.
func (s *PgStore) CreateBonusGrant(ctx context.Context, b *domain.BonusGrant) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO bonus_grants (
				grant_id, tenant_id, employee_id, bonus_type, amount,
				currency, grant_date, status, approved_by, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, b.GrantID, tenantID, b.EmployeeID, b.BonusType, b.Amount,
			b.Currency, b.GrantDate, b.Status, b.ApprovedBy, b.CorrelationID, b.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT grant_id, employee_id, bonus_type, amount, currency,
				       grant_date::text, status, approved_by, created_at
				FROM bonus_grants WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, b.CorrelationID)
			if err := row.Scan(
				&b.GrantID, &b.EmployeeID, &b.BonusType, &b.Amount, &b.Currency,
				&b.GrantDate, &b.Status, &b.ApprovedBy, &b.CreatedAt,
			); err != nil {
				return err
			}
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

// GetBonusGrant resolves a bonus grant by its primary key — used by the
// handler to authorize an approval against the grant's real employee
// (and thus legal entity) instead of listing every bonus grant in the
// tenant and scanning for a match in Go.
func (s *PgStore) GetBonusGrant(ctx context.Context, grantID string) (*domain.BonusGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var b domain.BonusGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT grant_id, tenant_id, employee_id, bonus_type, amount,
			       currency, grant_date::text, status, approved_by, created_at
			FROM bonus_grants WHERE grant_id = $1 AND tenant_id = $2
		`, grantID, tenantID).Scan(
			&b.GrantID, &b.TenantID, &b.EmployeeID, &b.BonusType, &b.Amount,
			&b.Currency, &b.GrantDate, &b.Status, &b.ApprovedBy, &b.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBonusNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
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
			       currency, grant_date::text, status, approved_by, created_at
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
