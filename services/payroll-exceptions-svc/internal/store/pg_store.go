package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payroll-exceptions-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-exceptions-svc/internal/middleware"
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

func (s *PgStore) CreateException(ctx context.Context, e *domain.PayrollException) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	details := e.DetailsJSON
	if details == "" {
		details = "{}"
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO payroll_exceptions (
				exception_id, tenant_id, payroll_run_id, employee_id, exception_code,
				severity, description, details_json, status, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, e.ExceptionID, tenantID, e.PayrollRunID, e.EmployeeID, e.ExceptionCode,
			e.Severity, e.Description, details, e.Status, e.CreatedAt)
		return err
	})
}

func (s *PgStore) GetException(ctx context.Context, exceptionID string) (*domain.PayrollException, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var e domain.PayrollException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT exception_id, tenant_id, payroll_run_id, employee_id, exception_code,
			       severity, description, details_json, status, resolution_notes,
			       resolved_by, resolved_at, created_at
			FROM payroll_exceptions
			WHERE tenant_id = $1 AND exception_id = $2
		`, tenantID, exceptionID).Scan(
			&e.ExceptionID, &e.TenantID, &e.PayrollRunID, &e.EmployeeID, &e.ExceptionCode,
			&e.Severity, &e.Description, &e.DetailsJSON, &e.Status, &e.ResolutionNotes,
			&e.ResolvedBy, &e.ResolvedAt, &e.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrExceptionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *PgStore) ListExceptions(ctx context.Context, payrollRunID, employeeID, status, severity string) ([]domain.PayrollException, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.PayrollException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT exception_id, tenant_id, payroll_run_id, employee_id, exception_code,
			       severity, description, details_json, status, resolution_notes,
			       resolved_by, resolved_at, created_at
			FROM payroll_exceptions
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if payrollRunID != "" {
			args = append(args, payrollRunID)
			query += fmt.Sprintf(" AND payroll_run_id = $%d", len(args))
		}
		if employeeID != "" {
			args = append(args, employeeID)
			query += fmt.Sprintf(" AND employee_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if severity != "" {
			args = append(args, severity)
			query += fmt.Sprintf(" AND severity = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e domain.PayrollException
			if err := rows.Scan(
				&e.ExceptionID, &e.TenantID, &e.PayrollRunID, &e.EmployeeID, &e.ExceptionCode,
				&e.Severity, &e.Description, &e.DetailsJSON, &e.Status, &e.ResolutionNotes,
				&e.ResolvedBy, &e.ResolvedAt, &e.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) ResolveException(ctx context.Context, exceptionID, notes, resolvedBy, newStatus string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE payroll_exceptions
			SET status = $1, resolution_notes = $2, resolved_by = $3, resolved_at = $4
			WHERE tenant_id = $5 AND exception_id = $6 AND status IN ('OPEN', 'IN_REVIEW')
		`, newStatus, notes, resolvedBy, now, tenantID, exceptionID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrAlreadyResolved
		}
		return nil
	})
}

func (s *PgStore) GetReleaseBlockers(ctx context.Context, payrollRunID string) (*domain.ReleaseBlockerSummary, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	summary := &domain.ReleaseBlockerSummary{
		PayrollRunID: payrollRunID,
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT 
				COUNT(*) AS total_exceptions,
				COUNT(*) FILTER (WHERE severity = 'BLOCKER' AND status IN ('OPEN', 'IN_REVIEW')) AS blocker_count,
				COUNT(*) FILTER (WHERE severity = 'WARNING' AND status IN ('OPEN', 'IN_REVIEW')) AS warning_count
			FROM payroll_exceptions
			WHERE tenant_id = $1 AND payroll_run_id = $2
		`, tenantID, payrollRunID).Scan(&summary.TotalExceptions, &summary.BlockerCount, &summary.WarningCount)
	})
	if err != nil {
		return nil, err
	}

	summary.CanRelease = (summary.BlockerCount == 0)
	return summary, nil
}