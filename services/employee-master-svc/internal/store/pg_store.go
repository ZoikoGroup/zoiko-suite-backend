package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/employee-master-svc/internal/domain"
	svcmiddleware "zoiko.io/employee-master-svc/internal/middleware"
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

func (s *PgStore) CreateEmployee(ctx context.Context, emp *domain.Employee) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO employees (
				employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name,
				email, phone, job_title, department_id, manager_employee_id, worker_type,
				status, hire_date, termination_date, effective_from, effective_to, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		`, emp.EmployeeID, tenantID, emp.LegalEntityID, emp.EmployeeNumber, emp.FirstName, emp.LastName,
			emp.Email, emp.Phone, emp.JobTitle, emp.DepartmentID, emp.ManagerEmployeeID, emp.WorkerType,
			emp.Status, emp.HireDate, emp.TerminationDate, emp.EffectiveFrom, emp.EffectiveTo, emp.CreatedAt, emp.UpdatedAt)
		return err
	})

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "idx_employees_tenant_number" {
			return domain.ErrEmployeeNumberExists
		}
		return domain.ErrEmailAlreadyExists
	}
	return err
}

func (s *PgStore) GetEmployee(ctx context.Context, id string) (*domain.Employee, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var emp domain.Employee
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name,
			       email, phone, job_title, department_id, manager_employee_id, worker_type,
			       status, hire_date, termination_date, effective_from, effective_to, created_at, updated_at
			FROM employees
			WHERE employee_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&emp.EmployeeID, &emp.TenantID, &emp.LegalEntityID, &emp.EmployeeNumber, &emp.FirstName, &emp.LastName,
			&emp.Email, &emp.Phone, &emp.JobTitle, &emp.DepartmentID, &emp.ManagerEmployeeID, &emp.WorkerType,
			&emp.Status, &emp.HireDate, &emp.TerminationDate, &emp.EffectiveFrom, &emp.EffectiveTo, &emp.CreatedAt, &emp.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEmployeeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &emp, nil
}

func (s *PgStore) ListEmployees(ctx context.Context, legalEntityID, status, workerType, departmentID, managerEmployeeID string) ([]domain.Employee, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Employee
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name,
			       email, phone, job_title, department_id, manager_employee_id, worker_type,
			       status, hire_date, termination_date, effective_from, effective_to, created_at, updated_at
			FROM employees
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if workerType != "" {
			args = append(args, workerType)
			query += fmt.Sprintf(" AND worker_type = $%d", len(args))
		}
		if departmentID != "" {
			args = append(args, departmentID)
			query += fmt.Sprintf(" AND department_id = $%d", len(args))
		}
		if managerEmployeeID != "" {
			args = append(args, managerEmployeeID)
			query += fmt.Sprintf(" AND manager_employee_id = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var emp domain.Employee
			if err := rows.Scan(
				&emp.EmployeeID, &emp.TenantID, &emp.LegalEntityID, &emp.EmployeeNumber, &emp.FirstName, &emp.LastName,
				&emp.Email, &emp.Phone, &emp.JobTitle, &emp.DepartmentID, &emp.ManagerEmployeeID, &emp.WorkerType,
				&emp.Status, &emp.HireDate, &emp.TerminationDate, &emp.EffectiveFrom, &emp.EffectiveTo, &emp.CreatedAt, &emp.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, emp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) UpdateEmployee(ctx context.Context, emp *domain.Employee) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE employees
			SET first_name = $1, last_name = $2, phone = $3, job_title = $4,
			    department_id = $5, manager_employee_id = $6, worker_type = $7, updated_at = $8
			WHERE employee_id = $9 AND tenant_id = $10
		`, emp.FirstName, emp.LastName, emp.Phone, emp.JobTitle,
			emp.DepartmentID, emp.ManagerEmployeeID, emp.WorkerType, emp.UpdatedAt,
			emp.EmployeeID, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrEmployeeNotFound
		}
		return nil
	})
}

func (s *PgStore) UpdateStatus(ctx context.Context, id, newStatus string, terminationDate *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var res pgconn.CommandTag
		var err error
		now := time.Now().UTC()

		if terminationDate != nil {
			res, err = tx.Exec(ctx, `
				UPDATE employees
				SET status = $1, termination_date = $2, effective_to = $3, updated_at = $3
				WHERE employee_id = $4 AND tenant_id = $5
			`, newStatus, terminationDate, now, id, tenantID)
		} else {
			res, err = tx.Exec(ctx, `
				UPDATE employees
				SET status = $1, updated_at = $2
				WHERE employee_id = $3 AND tenant_id = $4
			`, newStatus, now, id, tenantID)
		}
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrEmployeeNotFound
		}
		return nil
	})
}