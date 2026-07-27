package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/org-structure-svc/internal/domain"
	svcmiddleware "zoiko.io/org-structure-svc/internal/middleware"
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

func (s *PgStore) CreateDepartment(ctx context.Context, d *domain.Department) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO departments (
				department_id, tenant_id, legal_entity_id, name, code,
				cost_center_code, parent_department_id, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, d.DepartmentID, tenantID, d.LegalEntityID, d.Name, d.Code,
			d.CostCenterCode, d.ParentDepartmentID, d.Status, d.CreatedAt, d.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListDepartments(ctx context.Context, legalEntityID string) ([]domain.Department, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Department
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT department_id, tenant_id, legal_entity_id, name, code,
			       cost_center_code, parent_department_id, status, created_at, updated_at
			FROM departments
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
			var d domain.Department
			if err := rows.Scan(
				&d.DepartmentID, &d.TenantID, &d.LegalEntityID, &d.Name, &d.Code,
				&d.CostCenterCode, &d.ParentDepartmentID, &d.Status, &d.CreatedAt, &d.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, d)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetDepartment(ctx context.Context, departmentID string) (*domain.Department, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var d domain.Department
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT department_id, tenant_id, legal_entity_id, name, code,
			       cost_center_code, parent_department_id, status, created_at, updated_at
			FROM departments
			WHERE tenant_id = $1 AND department_id = $2
		`, tenantID, departmentID).Scan(
			&d.DepartmentID, &d.TenantID, &d.LegalEntityID, &d.Name, &d.Code,
			&d.CostCenterCode, &d.ParentDepartmentID, &d.Status, &d.CreatedAt, &d.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDepartmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *PgStore) CreatePosition(ctx context.Context, p *domain.Position) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO positions (
				position_id, tenant_id, legal_entity_id, department_id, title,
				code, job_level, max_headcount, current_headcount, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, p.PositionID, tenantID, p.LegalEntityID, p.DepartmentID, p.Title,
			p.Code, p.JobLevel, p.MaxHeadcount, p.CurrentHeadcount, p.Status, p.CreatedAt, p.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListPositions(ctx context.Context, departmentID string) ([]domain.Position, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Position
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT p.position_id, p.tenant_id, p.legal_entity_id, p.department_id, d.name,
			       p.title, p.code, p.job_level, p.max_headcount, p.current_headcount, p.status, p.created_at, p.updated_at
			FROM positions p
			JOIN departments d ON p.department_id = d.department_id
			WHERE p.tenant_id = $1
		`
		args := []any{tenantID}

		if departmentID != "" {
			args = append(args, departmentID)
			query += fmt.Sprintf(" AND p.department_id = $%d", len(args))
		}
		query += " ORDER BY p.title ASC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p domain.Position
			if err := rows.Scan(
				&p.PositionID, &p.TenantID, &p.LegalEntityID, &p.DepartmentID, &p.DepartmentName,
				&p.Title, &p.Code, &p.JobLevel, &p.MaxHeadcount, &p.CurrentHeadcount, &p.Status, &p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetPosition(ctx context.Context, positionID string) (*domain.Position, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var p domain.Position
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT p.position_id, p.tenant_id, p.legal_entity_id, p.department_id, d.name,
			       p.title, p.code, p.job_level, p.max_headcount, p.current_headcount, p.status, p.created_at, p.updated_at
			FROM positions p
			JOIN departments d ON p.department_id = d.department_id
			WHERE p.tenant_id = $1 AND p.position_id = $2
		`, tenantID, positionID).Scan(
			&p.PositionID, &p.TenantID, &p.LegalEntityID, &p.DepartmentID, &p.DepartmentName,
			&p.Title, &p.Code, &p.JobLevel, &p.MaxHeadcount, &p.CurrentHeadcount, &p.Status, &p.CreatedAt, &p.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPositionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) AssignEmployee(ctx context.Context, req *domain.AssignEmployeeRequest) (*domain.OrgAssignment, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	assignmentID := uuid.NewString()
	var oa domain.OrgAssignment

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. End-date existing active assignment
		_, err := tx.Exec(ctx, `
			UPDATE org_assignments
			SET effective_to = $1, status = 'SUPERSEDED', updated_at = $2
			WHERE tenant_id = $3 AND employee_id = $4 AND (effective_to IS NULL OR effective_to > $1)
		`, req.EffectiveFrom, now, tenantID, req.EmployeeID)
		if err != nil {
			return err
		}

		// 2. Create new assignment
		_, err = tx.Exec(ctx, `
			INSERT INTO org_assignments (
				assignment_id, tenant_id, employee_id, department_id, position_id,
				manager_employee_id, effective_from, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE', $8, $8)
		`, assignmentID, tenantID, req.EmployeeID, req.DepartmentID, req.PositionID,
			req.ManagerEmployeeID, req.EffectiveFrom, now)
		if err != nil {
			return err
		}

		// 3. Increment position headcount
		_, err = tx.Exec(ctx, `
			UPDATE positions
			SET current_headcount = current_headcount + 1, updated_at = $1
			WHERE tenant_id = $2 AND position_id = $3
		`, now, tenantID, req.PositionID)
		if err != nil {
			return err
		}

		return tx.QueryRow(ctx, `
			SELECT a.assignment_id, a.tenant_id, a.employee_id, a.department_id, d.name,
			       a.position_id, p.title, a.manager_employee_id, a.effective_from, a.effective_to,
			       a.status, a.created_at, a.updated_at
			FROM org_assignments a
			JOIN departments d ON a.department_id = d.department_id
			JOIN positions p ON a.position_id = p.position_id
			WHERE a.tenant_id = $1 AND a.assignment_id = $2
		`, tenantID, assignmentID).Scan(
			&oa.AssignmentID, &oa.TenantID, &oa.EmployeeID, &oa.DepartmentID, &oa.DepartmentName,
			&oa.PositionID, &oa.PositionTitle, &oa.ManagerEmployeeID, &oa.EffectiveFrom, &oa.EffectiveTo,
			&oa.Status, &oa.CreatedAt, &oa.UpdatedAt,
		)
	})
	if err != nil {
		return nil, err
	}
	return &oa, nil
}

func (s *PgStore) GetEmployeeAssignment(ctx context.Context, employeeID string) (*domain.OrgAssignment, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var oa domain.OrgAssignment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT a.assignment_id, a.tenant_id, a.employee_id, a.department_id, d.name,
			       a.position_id, p.title, a.manager_employee_id, a.effective_from, a.effective_to,
			       a.status, a.created_at, a.updated_at
			FROM org_assignments a
			JOIN departments d ON a.department_id = d.department_id
			JOIN positions p ON a.position_id = p.position_id
			WHERE a.tenant_id = $1 AND a.employee_id = $2 AND a.status = 'ACTIVE'
			ORDER BY a.effective_from DESC LIMIT 1
		`, tenantID, employeeID).Scan(
			&oa.AssignmentID, &oa.TenantID, &oa.EmployeeID, &oa.DepartmentID, &oa.DepartmentName,
			&oa.PositionID, &oa.PositionTitle, &oa.ManagerEmployeeID, &oa.EffectiveFrom, &oa.EffectiveTo,
			&oa.Status, &oa.CreatedAt, &oa.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAssignmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &oa, nil
}