package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

func taxedSlip(runID, tenant string, rule string, amounts ...float64) domain.PaySlip {
	slipID := uuid.NewString()
	now := time.Now().UTC()
	total := 0.0
	var lines []domain.PaySlipTaxLine
	types := []string{"STATE_FEDERAL", "SOCIAL_SECURITY", "HEALTH_STATUTORY"}
	names := []string{"Income Tax", "Social Insurance", "Medical Insurance"}
	for i, a := range amounts {
		total += a
		lines = append(lines, domain.PaySlipTaxLine{LineID: uuid.NewString(), TenantID: tenant, SlipID: slipID, Sequence: i + 1, TaxName: names[i],
			TaxType: types[i], RatePct: 5 + float64(i), Amount: a, CalculationID: "calc-" + slipID[:8], EngineType: "STANDARD_ENGINE",
			RuleVersion: rule, JurisdictionCode: "GB", CreatedAt: now})
	}
	return domain.PaySlip{SlipID: slipID, TenantID: tenant, RunID: runID, EmployeeID: uuid.NewString(), EmployeeNumber: "E1", EmployeeName: "A B",
		GrossPay: 1000, TaxWithheld: total, NetPay: 1000 - total, Currency: "GBP", EffectiveDate: "2026-02-05", TaxableAmount: 1000, CreatedAt: now, TaxLines: lines}
}

func TestTaxLinesAreSavedWithTheSlipAndReplacedWhenTheRunIsRecalculated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenant)
	run := newTestRun(tenant, "corr-tax-1")
	_, err := s.CreatePayrollRun(ctx, run)
	require.NoError(t, err)

	first := taxedSlip(run.RunID, tenant, "2026.1", 100, 50, 10)
	require.NoError(t, s.SaveCalculatedResults(ctx, run.RunID, 1000, 840, 160, 0, []domain.PaySlip{first}, nil))
	got, err := s.GetPaySlipsByRun(ctx, run.RunID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].TaxLines, 3)
	for i, l := range got[0].TaxLines {
		assert.Equal(t, first.TaxLines[i].LineID, l.LineID)
		assert.Equal(t, i+1, l.Sequence)
		assert.Equal(t, first.TaxLines[i].Amount, l.Amount)
		assert.Equal(t, "2026.1", l.RuleVersion)
		assert.Equal(t, "GB", l.JurisdictionCode)
		assert.Equal(t, first.SlipID, l.SlipID)
	}
	assert.Equal(t, "STATE_FEDERAL", got[0].TaxLines[0].TaxType)

	// Recalculation replaces the slips, and the lines go with them: nothing of the old calculation survives.
	second := taxedSlip(run.RunID, tenant, "2026.2", 200, 100)
	require.NoError(t, s.SaveCalculatedResults(ctx, run.RunID, 1000, 700, 300, 0, []domain.PaySlip{second}, nil))
	got, err = s.GetPaySlipsByRun(ctx, run.RunID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].TaxLines, 2)
	assert.Equal(t, "2026.2", got[0].TaxLines[0].RuleVersion)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM pay_slip_tax_lines WHERE slip_id = $1`, first.SlipID).Scan(&n))
	assert.Equal(t, 0, n, "the previous calculation's lines are gone")

	// A slip with no engine lines (the legacy placeholder) stores none and reads back none.
	legacy := taxedSlip(run.RunID, tenant, "x")
	legacy.TaxLines, legacy.TaxWithheld, legacy.NetPay = nil, 200, 800
	require.NoError(t, s.SaveCalculatedResults(ctx, run.RunID, 1000, 800, 200, 0, []domain.PaySlip{legacy}, nil))
	got, err = s.GetPaySlipsByRun(ctx, run.RunID)
	require.NoError(t, err)
	assert.Empty(t, got[0].TaxLines)
	assert.Equal(t, 0, domain.TaxSourceOf(got).EngineSlips)
	assert.Equal(t, 1, domain.TaxSourceOf(got).PlaceholderSlips)
}

func TestTaxLinesAreTenantIsolatedAndConstrained(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	ctxA, ctxB := middleware.WithTenant(context.Background(), tenantA), middleware.WithTenant(context.Background(), tenantB)
	run := newTestRun(tenantA, "corr-tax-iso")
	_, err := s.CreatePayrollRun(ctxA, run)
	require.NoError(t, err)
	slip := taxedSlip(run.RunID, tenantA, "2026.1", 100, 50)
	require.NoError(t, s.SaveCalculatedResults(ctxA, run.RunID, 1000, 850, 150, 0, []domain.PaySlip{slip}, nil))

	other, err := s.GetPaySlipsByRun(ctxB, run.RunID)
	require.NoError(t, err)
	assert.Empty(t, other, "another tenant sees neither the slips nor their tax")

	_, err = pool.Exec(context.Background(), `INSERT INTO pay_slip_tax_lines (line_id, tenant_id, slip_id, sequence, tax_name, tax_type, rate_pct, amount, calculation_id, engine_type, rule_version, jurisdiction_code, created_at)
		VALUES (gen_random_uuid(), $1, $2, 9, 'x', 'FEDERAL', 1, -1, 'c', 'e', 'v', 'GB', now())`, tenantA, slip.SlipID)
	assert.Error(t, err, "a negative tax component is refused by the database")
	_, err = pool.Exec(context.Background(), `INSERT INTO pay_slip_tax_lines (line_id, tenant_id, slip_id, sequence, tax_name, tax_type, rate_pct, amount, calculation_id, engine_type, rule_version, jurisdiction_code, created_at)
		VALUES (gen_random_uuid(), $1, $2, 1, 'dup', 'FEDERAL', 1, 1, 'c', 'e', 'v', 'GB', now())`, tenantA, slip.SlipID)
	assert.Error(t, err, "one line per sequence per slip")
	_, err = pool.Exec(context.Background(), `INSERT INTO pay_slip_tax_lines (line_id, tenant_id, slip_id, sequence, tax_name, tax_type, rate_pct, amount, calculation_id, engine_type, rule_version, jurisdiction_code, created_at)
		VALUES (gen_random_uuid(), $1, $2, 10, ' ', 'FEDERAL', 1, 1, 'c', 'e', 'v', 'GB', now())`, tenantA, slip.SlipID)
	assert.Error(t, err, "a tax line must name its tax")
}

func TestTaxLinesWorkUnderRowLevelSecurity(t *testing.T) {
	admin := openTestPool(t)
	app := appPool(t, admin)
	s := store.New(app, "")
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	ctxA, ctxB := middleware.WithTenant(context.Background(), tenantA), middleware.WithTenant(context.Background(), tenantB)
	run := newTestRun(tenantA, "corr-tax-rls")
	_, err := s.CreatePayrollRun(ctxA, run)
	require.NoError(t, err)
	slip := taxedSlip(run.RunID, tenantA, "2026.1", 100, 50, 10)
	require.NoError(t, s.SaveCalculatedResults(ctxA, run.RunID, 1000, 840, 160, 0, []domain.PaySlip{slip}, nil))
	got, err := s.GetPaySlipsByRun(ctxA, run.RunID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Len(t, got[0].TaxLines, 3)
	other, err := s.GetPaySlipsByRun(ctxB, run.RunID)
	require.NoError(t, err)
	assert.Empty(t, other)
	var n int
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM pay_slip_tax_lines`).Scan(&n))
	assert.Equal(t, 0, n, "no tenant context, no rows")
}
