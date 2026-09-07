package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/compensation-svc/internal/domain"
	svcmiddleware "zoiko.io/compensation-svc/internal/middleware"
)

const (
	statusActive   = "ACTIVE"
	statusInactive = "INACTIVE"
)

const componentColumns = `
	component_id, tenant_id, legal_entity_id, name, code, component_type,
	is_taxable, default_amount, currency, description, status, created_at, updated_at`

func scanComponent(row pgx.Row, c *domain.SalaryComponent) error {
	return row.Scan(
		&c.ComponentID, &c.TenantID, &c.LegalEntityID, &c.Name, &c.Code, &c.ComponentType,
		&c.IsTaxable, &c.DefaultAmount, &c.Currency, &c.Description, &c.Status, &c.CreatedAt, &c.UpdatedAt,
	)
}

func (s *PgStore) CreateComponent(ctx context.Context, c *domain.SalaryComponent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO salary_components (
				component_id, tenant_id, legal_entity_id, name, code, component_type,
				is_taxable, default_amount, currency, description, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, c.ComponentID, tenantID, c.LegalEntityID, c.Name, c.Code, c.ComponentType,
			c.IsTaxable, c.DefaultAmount, c.Currency, c.Description, c.Status, c.CreatedAt, c.UpdatedAt)
		return err
	})

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrComponentCodeExists
	}
	return err
}

func (s *PgStore) GetComponent(ctx context.Context, componentID string) (*domain.SalaryComponent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.SalaryComponent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanComponent(tx.QueryRow(ctx, `
			SELECT `+componentColumns+`
			FROM salary_components
			WHERE tenant_id = $1 AND component_id = $2
		`, tenantID, componentID), &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrComponentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) ListComponents(ctx context.Context, legalEntityID, componentType string, includeInactive bool) ([]domain.SalaryComponent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.SalaryComponent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + componentColumns + `
			FROM salary_components
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if componentType != "" {
			args = append(args, componentType)
			query += fmt.Sprintf(" AND component_type = $%d", len(args))
		}
		if !includeInactive {
			args = append(args, statusActive)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY component_type ASC, code ASC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.SalaryComponent
			if err := scanComponent(rows, &c); err != nil {
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

// DeactivateComponent retires a component. It is never hard-deleted: a payslip
// already issued against it must stay explicable, and structure_components
// holds a foreign key to it besides.
func (s *PgStore) DeactivateComponent(ctx context.Context, componentID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE salary_components
			SET status = $1, updated_at = NOW()
			WHERE tenant_id = $2 AND component_id = $3 AND status = $4
		`, statusInactive, tenantID, componentID, statusActive)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrComponentNotFound
		}
		return nil
	})
}

// GetStructure reads one compensation structure.
func (s *PgStore) GetStructure(ctx context.Context, structureID string) (*domain.CompensationStructure, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var str domain.CompensationStructure
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT structure_id, tenant_id, legal_entity_id, name, pay_type,
			       min_amount, max_amount, currency, overtime_multiplier, created_at, updated_at
			FROM compensation_structures
			WHERE tenant_id = $1 AND structure_id = $2
		`, tenantID, structureID).Scan(
			&str.StructureID, &str.TenantID, &str.LegalEntityID, &str.Name, &str.PayType,
			&str.MinAmount, &str.MaxAmount, &str.Currency, &str.OvertimeMultiplier, &str.CreatedAt, &str.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrStructureNotFound
	}
	if err != nil {
		return nil, err
	}
	return &str, nil
}

// SetStructureComponents replaces a structure's whole composition in one
// transaction. A partial write would leave a structure that computes a payslip
// nobody intended, so the old set is cleared and the new one inserted together
// or not at all.
func (s *PgStore) SetStructureComponents(ctx context.Context, structureID string, inputs []domain.StructureComponentInput) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			DELETE FROM structure_components
			WHERE tenant_id = $1 AND structure_id = $2
		`, tenantID, structureID); err != nil {
			return err
		}

		for _, in := range inputs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO structure_components (
					structure_component_id, tenant_id, structure_id, component_id,
					calculation_method, calculation_value, sequence, created_at, updated_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
			`, uuid.NewString(), tenantID, structureID, in.ComponentID,
				in.CalculationMethod, in.CalculationValue, in.Sequence); err != nil {
				return err
			}
		}
		return nil
	})

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return domain.ErrDuplicateComponent
		case "23503":
			// Foreign key: the component (or the structure) does not exist.
			return domain.ErrComponentNotFound
		}
	}
	return err
}

// ListStructureComponents returns a structure's composition in payslip order,
// joined to the component catalogue so a caller gets one round trip.
func (s *PgStore) ListStructureComponents(ctx context.Context, structureID string) ([]domain.StructureComponent, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.StructureComponent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT sc.structure_component_id, sc.tenant_id, sc.structure_id, sc.component_id,
			       sc.calculation_method, sc.calculation_value, sc.sequence,
			       c.name, c.code, c.component_type, c.is_taxable,
			       sc.created_at, sc.updated_at
			FROM structure_components sc
			JOIN salary_components c ON c.component_id = sc.component_id
			WHERE sc.tenant_id = $1 AND sc.structure_id = $2
			ORDER BY sc.sequence ASC, c.code ASC
		`, tenantID, structureID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var sc domain.StructureComponent
			if err := rows.Scan(
				&sc.StructureComponentID, &sc.TenantID, &sc.StructureID, &sc.ComponentID,
				&sc.CalculationMethod, &sc.CalculationValue, &sc.Sequence,
				&sc.ComponentName, &sc.ComponentCode, &sc.ComponentType, &sc.IsTaxable,
				&sc.CreatedAt, &sc.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
