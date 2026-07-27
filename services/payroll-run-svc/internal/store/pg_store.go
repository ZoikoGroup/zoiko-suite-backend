package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/payroll-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
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

// CreatePayrollRun inserts a payroll run in INITIATED status.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL run —
// mutating *r in place to reflect it — rather than creating a duplicate
// payroll run for the same period.
func (s *PgStore) CreatePayrollRun(ctx context.Context, r *domain.PayrollRun) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO payroll_runs (
				run_id, tenant_id, legal_entity_id, run_number, pay_period_start,
				pay_period_end, pay_date, status, is_shadow_run, total_gross_pay,
				total_net_pay, total_tax_deductions, total_other_deductions, employee_count,
				correlation_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, r.RunID, tenantID, r.LegalEntityID, r.RunNumber, r.PayPeriodStart,
			r.PayPeriodEnd, r.PayDate, r.Status, r.IsShadowRun, r.TotalGrossPay,
			r.TotalNetPay, r.TotalTaxDeductions, r.TotalOtherDeductions, r.EmployeeCount,
			r.CorrelationID, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT run_id, run_number, pay_period_start::text, pay_period_end::text, pay_date::text,
				       status, is_shadow_run, total_gross_pay, total_net_pay, total_tax_deductions,
				       total_other_deductions, employee_count, created_at, updated_at, finalized_at
				FROM payroll_runs WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, r.CorrelationID)
			if err := row.Scan(
				&r.RunID, &r.RunNumber, &r.PayPeriodStart, &r.PayPeriodEnd, &r.PayDate,
				&r.Status, &r.IsShadowRun, &r.TotalGrossPay, &r.TotalNetPay, &r.TotalTaxDeductions,
				&r.TotalOtherDeductions, &r.EmployeeCount, &r.CreatedAt, &r.UpdatedAt, &r.FinalizedAt,
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

func (s *PgStore) GetPayrollRun(ctx context.Context, id string) (*domain.PayrollRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var r domain.PayrollRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT run_id, tenant_id, legal_entity_id, run_number, pay_period_start::text,
			       pay_period_end::text, pay_date::text, status, is_shadow_run, total_gross_pay,
			       total_net_pay, total_tax_deductions, total_other_deductions, employee_count,
			       created_at, updated_at, finalized_at
			FROM payroll_runs
			WHERE run_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&r.RunID, &r.TenantID, &r.LegalEntityID, &r.RunNumber, &r.PayPeriodStart,
			&r.PayPeriodEnd, &r.PayDate, &r.Status, &r.IsShadowRun, &r.TotalGrossPay,
			&r.TotalNetPay, &r.TotalTaxDeductions, &r.TotalOtherDeductions, &r.EmployeeCount,
			&r.CreatedAt, &r.UpdatedAt, &r.FinalizedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPayrollRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PgStore) ListPayrollRuns(ctx context.Context, legalEntityID, status string, isShadowRun *bool) ([]domain.PayrollRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.PayrollRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT run_id, tenant_id, legal_entity_id, run_number, pay_period_start::text,
			       pay_period_end::text, pay_date::text, status, is_shadow_run, total_gross_pay,
			       total_net_pay, total_tax_deductions, total_other_deductions, employee_count,
			       created_at, updated_at, finalized_at
			FROM payroll_runs
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
		if isShadowRun != nil {
			args = append(args, *isShadowRun)
			query += fmt.Sprintf(" AND is_shadow_run = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r domain.PayrollRun
			if err := rows.Scan(
				&r.RunID, &r.TenantID, &r.LegalEntityID, &r.RunNumber, &r.PayPeriodStart,
				&r.PayPeriodEnd, &r.PayDate, &r.Status, &r.IsShadowRun, &r.TotalGrossPay,
				&r.TotalNetPay, &r.TotalTaxDeductions, &r.TotalOtherDeductions, &r.EmployeeCount,
				&r.CreatedAt, &r.UpdatedAt, &r.FinalizedAt,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) SaveCalculatedResults(ctx context.Context, runID string, totalGross, totalNet, totalTax, totalDeductions float64, slips []domain.PaySlip, shadowComps []domain.ShadowComparison) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Check run status (must not be COMPLETED)
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM payroll_runs WHERE run_id = $1 AND tenant_id = $2 FOR UPDATE", runID, tenantID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPayrollRunNotFound
		}
		if err != nil {
			return err
		}
		if status == "COMPLETED" {
			return domain.ErrRunAlreadyFinalized
		}

		// 2. Clear old calculated slips/shadow comparisons for this run
		if _, err := tx.Exec(ctx, "DELETE FROM pay_slips WHERE run_id = $1 AND tenant_id = $2", runID, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM shadow_payroll_comparisons WHERE run_id = $1 AND tenant_id = $2", runID, tenantID); err != nil {
			return err
		}

		// 3. Insert payslips
		for _, slip := range slips {
			_, err := tx.Exec(ctx, `
				INSERT INTO pay_slips (
					slip_id, tenant_id, run_id, employee_id, employee_number,
					employee_name, gross_pay, tax_withheld, benefits_deductions,
					net_pay, currency, effective_date, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			`, slip.SlipID, tenantID, runID, slip.EmployeeID, slip.EmployeeNumber,
				slip.EmployeeName, slip.GrossPay, slip.TaxWithheld, slip.BenefitsDeductions,
				slip.NetPay, slip.Currency, slip.EffectiveDate, slip.CreatedAt)
			if err != nil {
				return err
			}
		}

		// 4. Insert shadow comparisons
		for _, comp := range shadowComps {
			_, err := tx.Exec(ctx, `
				INSERT INTO shadow_payroll_comparisons (
					comparison_id, tenant_id, run_id, employee_id, legacy_gross_pay,
					legacy_net_pay, legacy_tax_withheld, zoiko_gross_pay, zoiko_net_pay,
					zoiko_tax_withheld, gross_variance, net_variance, tax_variance,
					is_equivalent, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			`, comp.ComparisonID, tenantID, runID, comp.EmployeeID, comp.LegacyGrossPay,
				comp.LegacyNetPay, comp.LegacyTaxWithheld, comp.ZoikoGrossPay, comp.ZoikoNetPay,
				comp.ZoikoTaxWithheld, comp.GrossVariance, comp.NetVariance, comp.TaxVariance,
				comp.IsEquivalent, comp.CreatedAt)
			if err != nil {
				return err
			}
		}

		// 5. Update run summary totals
		_, err = tx.Exec(ctx, `
			UPDATE payroll_runs
			SET status = 'CALCULATED',
			    total_gross_pay = $1,
			    total_net_pay = $2,
			    total_tax_deductions = $3,
			    total_other_deductions = $4,
			    employee_count = $5,
			    updated_at = $6
			WHERE run_id = $7 AND tenant_id = $8
		`, totalGross, totalNet, totalTax, totalDeductions, len(slips), time.Now().UTC(), runID, tenantID)
		return err
	})
}

func (s *PgStore) GetPaySlipsByRun(ctx context.Context, runID string) ([]domain.PaySlip, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.PaySlip
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT slip_id, tenant_id, run_id, employee_id, employee_number,
			       employee_name, gross_pay, tax_withheld, benefits_deductions,
			       net_pay, currency, effective_date::text, created_at
			FROM pay_slips
			WHERE run_id = $1 AND tenant_id = $2
			ORDER BY employee_name ASC
		`, runID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var slip domain.PaySlip
			if err := rows.Scan(
				&slip.SlipID, &slip.TenantID, &slip.RunID, &slip.EmployeeID, &slip.EmployeeNumber,
				&slip.EmployeeName, &slip.GrossPay, &slip.TaxWithheld, &slip.BenefitsDeductions,
				&slip.NetPay, &slip.Currency, &slip.EffectiveDate, &slip.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, slip)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetShadowComparisonsByRun(ctx context.Context, runID string) ([]domain.ShadowComparison, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ShadowComparison
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT comparison_id, tenant_id, run_id, employee_id, legacy_gross_pay,
			       legacy_net_pay, legacy_tax_withheld, zoiko_gross_pay, zoiko_net_pay,
			       zoiko_tax_withheld, gross_variance, net_variance, tax_variance,
			       is_equivalent, created_at
			FROM shadow_payroll_comparisons
			WHERE run_id = $1 AND tenant_id = $2
			ORDER BY created_at ASC
		`, runID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var comp domain.ShadowComparison
			if err := rows.Scan(
				&comp.ComparisonID, &comp.TenantID, &comp.RunID, &comp.EmployeeID, &comp.LegacyGrossPay,
				&comp.LegacyNetPay, &comp.LegacyTaxWithheld, &comp.ZoikoGrossPay, &comp.ZoikoNetPay,
				&comp.ZoikoTaxWithheld, &comp.GrossVariance, &comp.NetVariance, &comp.TaxVariance,
				&comp.IsEquivalent, &comp.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, comp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) FinalizePayrollRun(ctx context.Context, runID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM payroll_runs WHERE run_id = $1 AND tenant_id = $2 FOR UPDATE", runID, tenantID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPayrollRunNotFound
		}
		if err != nil {
			return err
		}

		if status == "COMPLETED" {
			return domain.ErrRunAlreadyFinalized
		}
		if status != "CALCULATED" {
			return domain.ErrRunNotCalculated
		}

		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			UPDATE payroll_runs
			SET status = 'COMPLETED',
			    updated_at = $1,
			    finalized_at = $2
			WHERE run_id = $3 AND tenant_id = $4
		`, now, now, runID, tenantID)
		return err
	})
}
