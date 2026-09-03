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

// fullColumns is the single-employee projection: everything the record holds,
// including personal profile and home address.
const fullColumns = `
	employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name,
	email, phone, job_title, department_id, manager_employee_id, worker_type,
	status, hire_date::text, termination_date::text, effective_from, effective_to,
	date_of_birth::text, gender, profile_picture_url, personal_email, work_email,
	current_address, permanent_address, city, state, country, postal_code,
	company, business_unit, division, team, designation_id, confirmation_date::text,
	created_at, updated_at`

// listColumns is the directory projection. It deliberately omits date of birth,
// gender, personal email, and both address fields: a caller listing a whole
// legal entity is building a directory or a headcount rollup, and neither needs
// personal data. Fetch one employee by id to get those.
const listColumns = `
	employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name,
	email, phone, job_title, department_id, manager_employee_id, worker_type,
	status, hire_date::text, termination_date::text, effective_from, effective_to,
	work_email, company, business_unit, division, team, designation_id, confirmation_date::text,
	created_at, updated_at`

type PgStore struct {
	pool *pgxpool.Pool

	// schema is applied as a transaction-local search_path on every withRLS
	// transaction. Empty means the server default, which is what a
	// database-per-service deployment wants.
	//
	// It is set per transaction rather than once per connection because neither
	// connection-level mechanism survives a transaction pooler:
	//
	//   - The DSN's " search_path=x" is a startup option, and Supavisor in
	//     transaction mode drops startup options - a pooled server connection
	//     is shared, so it cannot carry per-client session state.
	//   - ALTER ROLE ... SET search_path is applied by Postgres at SESSION
	//     start, and the pooler reuses server connections whose sessions began
	//     before the change. Measured: of four roles altered at the same
	//     moment, one picked it up and three were still on "$user", public
	//     five minutes and many connections later, with no way to force a
	//     recycle from the client.
	//
	// A transaction-local set_config is re-applied every transaction, so it
	// holds whichever pooled connection serves it.
	schema string
}

func New(pool *pgxpool.Pool, schema string) *PgStore {
	return &PgStore{pool: pool, schema: schema}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Before anything reads a table: an unqualified name resolves against
	// search_path, and the pooler cannot be relied on to have carried it.
	if s.schema != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", s.schema); err != nil {
			return fmt.Errorf("set search_path: %w", err)
		}
	}

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

// scanFull reads a fullColumns row into emp.
func scanFull(row pgx.Row, emp *domain.Employee) error {
	return row.Scan(
		&emp.EmployeeID, &emp.TenantID, &emp.LegalEntityID, &emp.EmployeeNumber, &emp.FirstName, &emp.LastName,
		&emp.Email, &emp.Phone, &emp.JobTitle, &emp.DepartmentID, &emp.ManagerEmployeeID, &emp.WorkerType,
		&emp.Status, &emp.HireDate, &emp.TerminationDate, &emp.EffectiveFrom, &emp.EffectiveTo,
		&emp.DateOfBirth, &emp.Gender, &emp.ProfilePictureURL, &emp.PersonalEmail, &emp.WorkEmail,
		&emp.CurrentAddress, &emp.PermanentAddress, &emp.City, &emp.State, &emp.Country, &emp.PostalCode,
		&emp.Company, &emp.BusinessUnit, &emp.Division, &emp.Team, &emp.DesignationID, &emp.ConfirmationDate,
		&emp.CreatedAt, &emp.UpdatedAt,
	)
}

// scanList reads a listColumns row into emp.
func scanList(row pgx.Row, emp *domain.Employee) error {
	return row.Scan(
		&emp.EmployeeID, &emp.TenantID, &emp.LegalEntityID, &emp.EmployeeNumber, &emp.FirstName, &emp.LastName,
		&emp.Email, &emp.Phone, &emp.JobTitle, &emp.DepartmentID, &emp.ManagerEmployeeID, &emp.WorkerType,
		&emp.Status, &emp.HireDate, &emp.TerminationDate, &emp.EffectiveFrom, &emp.EffectiveTo,
		&emp.WorkEmail, &emp.Company, &emp.BusinessUnit, &emp.Division, &emp.Team, &emp.DesignationID, &emp.ConfirmationDate,
		&emp.CreatedAt, &emp.UpdatedAt,
	)
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
				status, hire_date, termination_date, effective_from, effective_to,
				date_of_birth, gender, profile_picture_url, personal_email, work_email,
				current_address, permanent_address, city, state, country, postal_code,
				company, business_unit, division, team, designation_id, confirmation_date,
				created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
				$19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36
			)
		`, emp.EmployeeID, tenantID, emp.LegalEntityID, emp.EmployeeNumber, emp.FirstName, emp.LastName,
			emp.Email, emp.Phone, emp.JobTitle, emp.DepartmentID, emp.ManagerEmployeeID, emp.WorkerType,
			emp.Status, emp.HireDate, emp.TerminationDate, emp.EffectiveFrom, emp.EffectiveTo,
			emp.DateOfBirth, emp.Gender, emp.ProfilePictureURL, emp.PersonalEmail, emp.WorkEmail,
			emp.CurrentAddress, emp.PermanentAddress, emp.City, emp.State, emp.Country, emp.PostalCode,
			emp.Company, emp.BusinessUnit, emp.Division, emp.Team, emp.DesignationID, emp.ConfirmationDate,
			emp.CreatedAt, emp.UpdatedAt)
		return err
	})

	return mapUniqueViolation(err)
}

// mapUniqueViolation turns a Postgres 23505 into the domain error naming the
// column that actually collided, so the handler can pick the right status code
// and tell the caller which field to fix.
func mapUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "idx_employees_tenant_number":
		return domain.ErrEmployeeNumberExists
	case "idx_employees_tenant_work_email":
		return domain.ErrWorkEmailAlreadyExists
	default:
		return domain.ErrEmailAlreadyExists
	}
}

func (s *PgStore) GetEmployee(ctx context.Context, id string) (*domain.Employee, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var emp domain.Employee
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanFull(tx.QueryRow(ctx, `
			SELECT `+fullColumns+`
			FROM employees
			WHERE employee_id = $1 AND tenant_id = $2
		`, id, tenantID), &emp)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEmployeeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &emp, nil
}

func (s *PgStore) ListEmployees(ctx context.Context, f domain.EmployeeFilter) ([]domain.Employee, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Employee
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + listColumns + `
			FROM employees
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		for _, c := range []struct {
			column string
			value  string
		}{
			{"legal_entity_id", f.LegalEntityID},
			{"status", f.Status},
			{"worker_type", f.WorkerType},
			{"department_id", f.DepartmentID},
			{"manager_employee_id", f.ManagerEmployeeID},
			{"business_unit", f.BusinessUnit},
			{"division", f.Division},
			{"designation_id", f.DesignationID},
		} {
			if c.value == "" {
				continue
			}
			args = append(args, c.value)
			query += fmt.Sprintf(" AND %s = $%d", c.column, len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var emp domain.Employee
			if err := scanList(rows, &emp); err != nil {
				return err
			}
			out = append(out, emp)
		}
		return rows.Err()
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

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE employees
			SET first_name = $1, last_name = $2, phone = $3, job_title = $4,
			    department_id = $5, manager_employee_id = $6, worker_type = $7,
			    date_of_birth = $8, gender = $9, profile_picture_url = $10,
			    personal_email = $11, work_email = $12,
			    current_address = $13, permanent_address = $14, city = $15,
			    state = $16, country = $17, postal_code = $18,
			    company = $19, business_unit = $20, division = $21, team = $22,
			    designation_id = $23, confirmation_date = $24, updated_at = $25
			WHERE employee_id = $26 AND tenant_id = $27
		`, emp.FirstName, emp.LastName, emp.Phone, emp.JobTitle,
			emp.DepartmentID, emp.ManagerEmployeeID, emp.WorkerType,
			emp.DateOfBirth, emp.Gender, emp.ProfilePictureURL,
			emp.PersonalEmail, emp.WorkEmail,
			emp.CurrentAddress, emp.PermanentAddress, emp.City,
			emp.State, emp.Country, emp.PostalCode,
			emp.Company, emp.BusinessUnit, emp.Division, emp.Team,
			emp.DesignationID, emp.ConfirmationDate, emp.UpdatedAt,
			emp.EmployeeID, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrEmployeeNotFound
		}
		return nil
	})

	return mapUniqueViolation(err)
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
