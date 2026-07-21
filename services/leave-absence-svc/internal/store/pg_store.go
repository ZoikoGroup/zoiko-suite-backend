package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/leave-absence-svc/internal/domain"
	svcmiddleware "zoiko.io/leave-absence-svc/internal/middleware"
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

func (s *PgStore) CreateLeaveType(ctx context.Context, lt *domain.LeaveType) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO leave_types (
				leave_type_id, tenant_id, legal_entity_id, name, code,
				is_paid, accrual_rate_per_year, max_balance, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, lt.LeaveTypeID, tenantID, lt.LegalEntityID, lt.Name, lt.Code,
			lt.IsPaid, lt.AccrualRatePerYear, lt.MaxBalance, lt.Status, lt.CreatedAt, lt.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListLeaveTypes(ctx context.Context, legalEntityID string) ([]domain.LeaveType, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.LeaveType
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT leave_type_id, tenant_id, legal_entity_id, name, code,
			       is_paid, accrual_rate_per_year, max_balance, status, created_at, updated_at
			FROM leave_types
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		query += " ORDER BY name ASC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var lt domain.LeaveType
			if err := rows.Scan(
				&lt.LeaveTypeID, &lt.TenantID, &lt.LegalEntityID, &lt.Name, &lt.Code,
				&lt.IsPaid, &lt.AccrualRatePerYear, &lt.MaxBalance, &lt.Status, &lt.CreatedAt, &lt.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, lt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetLeaveType(ctx context.Context, leaveTypeID string) (*domain.LeaveType, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var lt domain.LeaveType
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT leave_type_id, tenant_id, legal_entity_id, name, code,
			       is_paid, accrual_rate_per_year, max_balance, status, created_at, updated_at
			FROM leave_types
			WHERE tenant_id = $1 AND leave_type_id = $2
		`, tenantID, leaveTypeID).Scan(
			&lt.LeaveTypeID, &lt.TenantID, &lt.LegalEntityID, &lt.Name, &lt.Code,
			&lt.IsPaid, &lt.AccrualRatePerYear, &lt.MaxBalance, &lt.Status, &lt.CreatedAt, &lt.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrLeaveTypeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &lt, nil
}

func (s *PgStore) GetLeaveBalances(ctx context.Context, employeeID string) ([]domain.LeaveBalance, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.LeaveBalance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT b.balance_id, b.tenant_id, b.employee_id, b.leave_type_id,
			       t.name, t.code, b.allocated_hours, b.used_hours, b.pending_hours, b.updated_at
			FROM leave_balances b
			JOIN leave_types t ON b.leave_type_id = t.leave_type_id
			WHERE b.tenant_id = $1 AND b.employee_id = $2
		`, tenantID, employeeID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var b domain.LeaveBalance
			if err := rows.Scan(
				&b.BalanceID, &b.TenantID, &b.EmployeeID, &b.LeaveTypeID,
				&b.LeaveTypeName, &b.LeaveTypeCode, &b.AllocatedHours, &b.UsedHours, &b.PendingHours, &b.UpdatedAt,
			); err != nil {
				return err
			}
			b.AvailableHours = b.AllocatedHours - b.UsedHours - b.PendingHours
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) AccrueLeaveBalance(ctx context.Context, employeeID, leaveTypeID string, hours float64) (*domain.LeaveBalance, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	var b domain.LeaveBalance

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		balanceID := uuid.NewString()
		_, err := tx.Exec(ctx, `
			INSERT INTO leave_balances (balance_id, tenant_id, employee_id, leave_type_id, allocated_hours, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, employee_id, leave_type_id)
			DO UPDATE SET allocated_hours = leave_balances.allocated_hours + $5, updated_at = $6
		`, balanceID, tenantID, employeeID, leaveTypeID, hours, now)
		if err != nil {
			return err
		}

		return tx.QueryRow(ctx, `
			SELECT b.balance_id, b.tenant_id, b.employee_id, b.leave_type_id,
			       t.name, t.code, b.allocated_hours, b.used_hours, b.pending_hours, b.updated_at
			FROM leave_balances b
			JOIN leave_types t ON b.leave_type_id = t.leave_type_id
			WHERE b.tenant_id = $1 AND b.employee_id = $2 AND b.leave_type_id = $3
		`, tenantID, employeeID, leaveTypeID).Scan(
			&b.BalanceID, &b.TenantID, &b.EmployeeID, &b.LeaveTypeID,
			&b.LeaveTypeName, &b.LeaveTypeCode, &b.AllocatedHours, &b.UsedHours, &b.PendingHours, &b.UpdatedAt,
		)
	})
	if err != nil {
		return nil, err
	}
	b.AvailableHours = b.AllocatedHours - b.UsedHours - b.PendingHours
	return &b, nil
}

func (s *PgStore) SubmitLeaveRequest(ctx context.Context, req *domain.SubmitLeaveRequest) (*domain.LeaveRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	requestID := uuid.NewString()
	var lr domain.LeaveRequest

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Fetch balance
		var allocated, used, pending float64
		err := tx.QueryRow(ctx, `
			SELECT allocated_hours, used_hours, pending_hours
			FROM leave_balances
			WHERE tenant_id = $1 AND employee_id = $2 AND leave_type_id = $3
		`, tenantID, req.EmployeeID, req.LeaveTypeID).Scan(&allocated, &used, &pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInsufficientBalance
		}
		if err != nil {
			return err
		}

		available := allocated - used - pending
		if available < req.TotalHours {
			return domain.ErrInsufficientBalance
		}

		// 2. Lock pending hours
		_, err = tx.Exec(ctx, `
			UPDATE leave_balances
			SET pending_hours = pending_hours + $1, updated_at = $2
			WHERE tenant_id = $3 AND employee_id = $4 AND leave_type_id = $5
		`, req.TotalHours, now, tenantID, req.EmployeeID, req.LeaveTypeID)
		if err != nil {
			return err
		}

		// 3. Create request
		_, err = tx.Exec(ctx, `
			INSERT INTO leave_requests (
				request_id, tenant_id, employee_id, leave_type_id, start_date,
				end_date, total_hours, reason, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'SUBMITTED', $9, $9)
		`, requestID, tenantID, req.EmployeeID, req.LeaveTypeID, req.StartDate,
			req.EndDate, req.TotalHours, req.Reason, now)
		if err != nil {
			return err
		}

		return tx.QueryRow(ctx, `
			SELECT r.request_id, r.tenant_id, r.employee_id, r.leave_type_id, t.name,
			       r.start_date, r.end_date, r.total_hours, r.reason, r.status,
			       r.reviewer_id, r.reviewer_notes, r.reviewed_at, r.created_at, r.updated_at
			FROM leave_requests r
			JOIN leave_types t ON r.leave_type_id = t.leave_type_id
			WHERE r.tenant_id = $1 AND r.request_id = $2
		`, tenantID, requestID).Scan(
			&lr.RequestID, &lr.TenantID, &lr.EmployeeID, &lr.LeaveTypeID, &lr.LeaveTypeName,
			&lr.StartDate, &lr.EndDate, &lr.TotalHours, &lr.Reason, &lr.Status,
			&lr.ReviewerID, &lr.ReviewerNotes, &lr.ReviewedAt, &lr.CreatedAt, &lr.UpdatedAt,
		)
	})
	if err != nil {
		return nil, err
	}
	return &lr, nil
}

func (s *PgStore) GetLeaveRequest(ctx context.Context, requestID string) (*domain.LeaveRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var lr domain.LeaveRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT r.request_id, r.tenant_id, r.employee_id, r.leave_type_id, t.name,
			       r.start_date, r.end_date, r.total_hours, r.reason, r.status,
			       r.reviewer_id, r.reviewer_notes, r.reviewed_at, r.created_at, r.updated_at
			FROM leave_requests r
			JOIN leave_types t ON r.leave_type_id = t.leave_type_id
			WHERE r.tenant_id = $1 AND r.request_id = $2
		`, tenantID, requestID).Scan(
			&lr.RequestID, &lr.TenantID, &lr.EmployeeID, &lr.LeaveTypeID, &lr.LeaveTypeName,
			&lr.StartDate, &lr.EndDate, &lr.TotalHours, &lr.Reason, &lr.Status,
			&lr.ReviewerID, &lr.ReviewerNotes, &lr.ReviewedAt, &lr.CreatedAt, &lr.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	return &lr, nil
}

func (s *PgStore) ListLeaveRequests(ctx context.Context, employeeID, status string) ([]domain.LeaveRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.LeaveRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT r.request_id, r.tenant_id, r.employee_id, r.leave_type_id, t.name,
			       r.start_date, r.end_date, r.total_hours, r.reason, r.status,
			       r.reviewer_id, r.reviewer_notes, r.reviewed_at, r.created_at, r.updated_at
			FROM leave_requests r
			JOIN leave_types t ON r.leave_type_id = t.leave_type_id
			WHERE r.tenant_id = $1
		`
		args := []any{tenantID}

		if employeeID != "" {
			args = append(args, employeeID)
			query += fmt.Sprintf(" AND r.employee_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND r.status = $%d", len(args))
		}
		query += " ORDER BY r.created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var lr domain.LeaveRequest
			if err := rows.Scan(
				&lr.RequestID, &lr.TenantID, &lr.EmployeeID, &lr.LeaveTypeID, &lr.LeaveTypeName,
				&lr.StartDate, &lr.EndDate, &lr.TotalHours, &lr.Reason, &lr.Status,
				&lr.ReviewerID, &lr.ReviewerNotes, &lr.ReviewedAt, &lr.CreatedAt, &lr.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, lr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) ApproveLeaveRequest(ctx context.Context, requestID, reviewerID, notes string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var empID, leaveTypeID string
		var totalHours float64
		err := tx.QueryRow(ctx, `
			SELECT employee_id, leave_type_id, total_hours
			FROM leave_requests
			WHERE tenant_id = $1 AND request_id = $2 AND status = 'SUBMITTED'
		`, tenantID, requestID).Scan(&empID, &leaveTypeID, &totalHours)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidStatusTransition
		}
		if err != nil {
			return err
		}

		// 1. Move pending hours to used hours
		_, err = tx.Exec(ctx, `
			UPDATE leave_balances
			SET pending_hours = pending_hours - $1, used_hours = used_hours + $1, updated_at = $2
			WHERE tenant_id = $3 AND employee_id = $4 AND leave_type_id = $5
		`, totalHours, now, tenantID, empID, leaveTypeID)
		if err != nil {
			return err
		}

		// 2. Update request status
		_, err = tx.Exec(ctx, `
			UPDATE leave_requests
			SET status = 'APPROVED', reviewer_id = $1, reviewer_notes = $2, reviewed_at = $3, updated_at = $3
			WHERE tenant_id = $4 AND request_id = $5
		`, reviewerID, notes, now, tenantID, requestID)
		return err
	})
}

func (s *PgStore) RejectLeaveRequest(ctx context.Context, requestID, reviewerID, notes string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var empID, leaveTypeID string
		var totalHours float64
		err := tx.QueryRow(ctx, `
			SELECT employee_id, leave_type_id, total_hours
			FROM leave_requests
			WHERE tenant_id = $1 AND request_id = $2 AND status = 'SUBMITTED'
		`, tenantID, requestID).Scan(&empID, &leaveTypeID, &totalHours)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidStatusTransition
		}
		if err != nil {
			return err
		}

		// 1. Release pending hours
		_, err = tx.Exec(ctx, `
			UPDATE leave_balances
			SET pending_hours = pending_hours - $1, updated_at = $2
			WHERE tenant_id = $3 AND employee_id = $4 AND leave_type_id = $5
		`, totalHours, now, tenantID, empID, leaveTypeID)
		if err != nil {
			return err
		}

		// 2. Update request status
		_, err = tx.Exec(ctx, `
			UPDATE leave_requests
			SET status = 'REJECTED', reviewer_id = $1, reviewer_notes = $2, reviewed_at = $3, updated_at = $3
			WHERE tenant_id = $4 AND request_id = $5
		`, reviewerID, notes, now, tenantID, requestID)
		return err
	})
}