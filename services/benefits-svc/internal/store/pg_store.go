package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/benefits-svc/internal/domain"
	svcmiddleware "zoiko.io/benefits-svc/internal/middleware"
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

func (s *PgStore) CreatePlan(ctx context.Context, p *domain.BenefitPlan) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO benefit_plans (
				plan_id, tenant_id, legal_entity_id, name, plan_type,
				provider_name, deduction_tax_treatment, employer_contribution_pct,
				employee_contribution_amount, currency, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, p.PlanID, tenantID, p.LegalEntityID, p.Name, p.PlanType,
			p.ProviderName, p.DeductionTaxTreatment, p.EmployerContributionPct,
			p.EmployeeContributionAmount, p.Currency, p.Status, p.CreatedAt, p.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListPlans(ctx context.Context, legalEntityID, status string) ([]domain.BenefitPlan, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BenefitPlan
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT plan_id, tenant_id, legal_entity_id, name, plan_type,
			       provider_name, deduction_tax_treatment, employer_contribution_pct,
			       employee_contribution_amount, currency, status, created_at, updated_at
			FROM benefit_plans
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
		query += " ORDER BY name ASC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p domain.BenefitPlan
			if err := rows.Scan(
				&p.PlanID, &p.TenantID, &p.LegalEntityID, &p.Name, &p.PlanType,
				&p.ProviderName, &p.DeductionTaxTreatment, &p.EmployerContributionPct,
				&p.EmployeeContributionAmount, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt,
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

func (s *PgStore) GetPlan(ctx context.Context, planID string) (*domain.BenefitPlan, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var p domain.BenefitPlan
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT plan_id, tenant_id, legal_entity_id, name, plan_type,
			       provider_name, deduction_tax_treatment, employer_contribution_pct,
			       employee_contribution_amount, currency, status, created_at, updated_at
			FROM benefit_plans
			WHERE tenant_id = $1 AND plan_id = $2
		`, tenantID, planID).Scan(
			&p.PlanID, &p.TenantID, &p.LegalEntityID, &p.Name, &p.PlanType,
			&p.ProviderName, &p.DeductionTaxTreatment, &p.EmployerContributionPct,
			&p.EmployeeContributionAmount, &p.Currency, &p.Status, &p.CreatedAt, &p.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPlanNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) CreateElection(ctx context.Context, e *domain.BenefitElection) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO benefit_elections (
				election_id, tenant_id, employee_id, plan_id, coverage_level,
				employee_contribution_amount, employer_contribution_amount,
				effective_from, effective_to, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, e.ElectionID, tenantID, e.EmployeeID, e.PlanID, e.CoverageLevel,
			e.EmployeeContributionAmount, e.EmployerContributionAmount,
			e.EffectiveFrom, e.EffectiveTo, e.Status, e.CreatedAt, e.UpdatedAt)
		return err
	})
}

func (s *PgStore) UpdateElection(ctx context.Context, e *domain.BenefitElection) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE benefit_elections
			SET coverage_level = $1, employee_contribution_amount = $2,
			    employer_contribution_amount = $3, updated_at = $4
			WHERE tenant_id = $5 AND election_id = $6 AND status = 'ACTIVE'
		`, e.CoverageLevel, e.EmployeeContributionAmount, e.EmployerContributionAmount, e.UpdatedAt, tenantID, e.ElectionID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrElectionNotFound
		}
		return nil
	})
}

func (s *PgStore) CancelElection(ctx context.Context, electionID, cancelDate string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM benefit_elections WHERE election_id = $1 AND tenant_id = $2 FOR UPDATE", electionID, tenantID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrElectionNotFound
		}
		if err != nil {
			return err
		}

		if status == "CANCELLED" {
			return domain.ErrElectionAlreadyCancelled
		}

		_, err = tx.Exec(ctx, `
			UPDATE benefit_elections
			SET status = 'CANCELLED', effective_to = $1
			WHERE election_id = $2 AND tenant_id = $3
		`, cancelDate, electionID, tenantID)
		return err
	})
}

func (s *PgStore) ListElectionsByEmployee(ctx context.Context, employeeID, status string) ([]domain.BenefitElection, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BenefitElection
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT election_id, tenant_id, employee_id, plan_id, coverage_level,
			       employee_contribution_amount, employer_contribution_amount,
			       effective_from, effective_to, status, created_at, updated_at
			FROM benefit_elections
			WHERE tenant_id = $1 AND employee_id = $2
		`
		args := []any{tenantID, employeeID}

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
			var e domain.BenefitElection
			if err := rows.Scan(
				&e.ElectionID, &e.TenantID, &e.EmployeeID, &e.PlanID, &e.CoverageLevel,
				&e.EmployeeContributionAmount, &e.EmployerContributionAmount,
				&e.EffectiveFrom, &e.EffectiveTo, &e.Status, &e.CreatedAt, &e.UpdatedAt,
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