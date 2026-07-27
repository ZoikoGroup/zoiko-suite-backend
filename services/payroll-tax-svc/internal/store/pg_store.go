package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payroll-tax-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-tax-svc/internal/middleware"
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

func (s *PgStore) CreateProfile(ctx context.Context, p *domain.TaxJurisdictionProfile) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tax_jurisdiction_profiles (
				profile_id, tenant_id, legal_entity_id, jurisdiction_code,
				tax_engine_type, provider_endpoint, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, p.ProfileID, tenantID, p.LegalEntityID, p.JurisdictionCode,
			p.TaxEngineType, p.ProviderEndpoint, p.Status, p.CreatedAt, p.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListProfiles(ctx context.Context, legalEntityID, jurisdictionCode string) ([]domain.TaxJurisdictionProfile, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.TaxJurisdictionProfile
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT profile_id, tenant_id, legal_entity_id, jurisdiction_code,
			       tax_engine_type, provider_endpoint, status, created_at, updated_at
			FROM tax_jurisdiction_profiles
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if jurisdictionCode != "" {
			args = append(args, jurisdictionCode)
			query += fmt.Sprintf(" AND jurisdiction_code = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p domain.TaxJurisdictionProfile
			if err := rows.Scan(
				&p.ProfileID, &p.TenantID, &p.LegalEntityID, &p.JurisdictionCode,
				&p.TaxEngineType, &p.ProviderEndpoint, &p.Status, &p.CreatedAt, &p.UpdatedAt,
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

func (s *PgStore) CreateCalculationWithAudit(ctx context.Context, calc *domain.TaxCalculationRecord, audit *domain.TaxBasisAudit) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	breakdownRaw, err := json.Marshal(calc.TaxBreakdown)
	if err != nil {
		return fmt.Errorf("marshal tax breakdown: %w", err)
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Insert calculation record
		_, err := tx.Exec(ctx, `
			INSERT INTO tax_calculation_records (
				calculation_id, tenant_id, payroll_run_id, employee_id, jurisdiction_code,
				gross_taxable_amount, pre_tax_deduction_amount, taxable_basis, total_tax_amount,
				tax_breakdown, engine_type, rule_version_used, status, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`, calc.CalculationID, tenantID, calc.PayrollRunID, calc.EmployeeID, calc.JurisdictionCode,
			calc.GrossTaxableAmount, calc.PreTaxDeductionAmount, calc.TaxableBasis, calc.TotalTaxAmount,
			breakdownRaw, calc.EngineType, calc.RuleVersionUsed, calc.Status, calc.CreatedAt)
		if err != nil {
			return err
		}

		// 2. Insert audit log
		_, err = tx.Exec(ctx, `
			INSERT INTO tax_basis_audits (
				audit_id, tenant_id, calculation_id, employee_id,
				rule_basis_json, provider_metadata_json, audited_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, audit.AuditID, tenantID, audit.CalculationID, audit.EmployeeID,
			audit.RuleBasisJSON, audit.ProviderMetadataJSON, audit.AuditedAt)
		return err
	})
}

func (s *PgStore) GetCalculation(ctx context.Context, calculationID string) (*domain.TaxCalculationRecord, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var calc domain.TaxCalculationRecord
	var breakdownRaw []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT calculation_id, tenant_id, payroll_run_id, employee_id, jurisdiction_code,
			       gross_taxable_amount, pre_tax_deduction_amount, taxable_basis, total_tax_amount,
			       tax_breakdown, engine_type, rule_version_used, status, created_at
			FROM tax_calculation_records
			WHERE tenant_id = $1 AND calculation_id = $2
		`, tenantID, calculationID).Scan(
			&calc.CalculationID, &calc.TenantID, &calc.PayrollRunID, &calc.EmployeeID, &calc.JurisdictionCode,
			&calc.GrossTaxableAmount, &calc.PreTaxDeductionAmount, &calc.TaxableBasis, &calc.TotalTaxAmount,
			&breakdownRaw, &calc.EngineType, &calc.RuleVersionUsed, &calc.Status, &calc.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCalculationNotFound
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(breakdownRaw, &calc.TaxBreakdown); err != nil {
		return nil, fmt.Errorf("unmarshal tax breakdown: %w", err)
	}
	return &calc, nil
}

func (s *PgStore) ListCalculations(ctx context.Context, payrollRunID, employeeID string) ([]domain.TaxCalculationRecord, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.TaxCalculationRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT calculation_id, tenant_id, payroll_run_id, employee_id, jurisdiction_code,
			       gross_taxable_amount, pre_tax_deduction_amount, taxable_basis, total_tax_amount,
			       tax_breakdown, engine_type, rule_version_used, status, created_at
			FROM tax_calculation_records
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
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var calc domain.TaxCalculationRecord
			var breakdownRaw []byte
			if err := rows.Scan(
				&calc.CalculationID, &calc.TenantID, &calc.PayrollRunID, &calc.EmployeeID, &calc.JurisdictionCode,
				&calc.GrossTaxableAmount, &calc.PreTaxDeductionAmount, &calc.TaxableBasis, &calc.TotalTaxAmount,
				&breakdownRaw, &calc.EngineType, &calc.RuleVersionUsed, &calc.Status, &calc.CreatedAt,
			); err != nil {
				return err
			}
			_ = json.Unmarshal(breakdownRaw, &calc.TaxBreakdown)
			out = append(out, calc)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetTaxBasisAudit(ctx context.Context, calculationID string) (*domain.TaxBasisAudit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var audit domain.TaxBasisAudit
	var ruleBasisRaw, providerMetaRaw []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT audit_id, tenant_id, calculation_id, employee_id,
			       rule_basis_json, provider_metadata_json, audited_at
			FROM tax_basis_audits
			WHERE tenant_id = $1 AND calculation_id = $2
		`, tenantID, calculationID).Scan(
			&audit.AuditID, &audit.TenantID, &audit.CalculationID, &audit.EmployeeID,
			&ruleBasisRaw, &providerMetaRaw, &audit.AuditedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAuditNotFound
	}
	if err != nil {
		return nil, err
	}

	audit.RuleBasisJSON = string(ruleBasisRaw)
	audit.ProviderMetadataJSON = string(providerMetaRaw)
	return &audit, nil
}

func (s *PgStore) AdjustCalculation(ctx context.Context, calculationID string, newBreakdown []domain.TaxComponent, newTotalTax float64, reason string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	breakdownRaw, err := json.Marshal(newBreakdown)
	if err != nil {
		return fmt.Errorf("marshal tax breakdown: %w", err)
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE tax_calculation_records
			SET status = 'ADJUSTED', total_tax_amount = $1, tax_breakdown = $2
			WHERE tenant_id = $3 AND calculation_id = $4
		`, newTotalTax, breakdownRaw, tenantID, calculationID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrCalculationNotFound
		}
		return nil
	})
}