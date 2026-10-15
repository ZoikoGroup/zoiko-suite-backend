package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func line(worker, kind, category, treatment, code, amount, currency string) domain.InputLine {
	tt := treatment
	pc := code
	return domain.InputLine{LineID: worker + code, WorkerID: worker, SourceKind: kind, InputCode: code, PayrollCode: &pc, Category: category,
		TaxTreatment: &tt, Amount: amount, Currency: currency, State: domain.InputLineAccepted}
}

func TestBuildBatchPay_GrossTaxableDeductionsAndItemsAreExact(t *testing.T) {
	lines := []domain.InputLine{
		line("w1", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "BASE", "3000.0000", "GBP"),
		line("w1", domain.SourceCompComponent, domain.CategoryEarning, domain.TaxTaxable, "HRA", "100.5000", "GBP"),
		line("w1", domain.SourceCompComponent, domain.CategoryEarning, domain.TaxNonTaxable, "TRANSPORT", "50.0000", "GBP"),
		line("w1", domain.SourceCompBonus, domain.CategoryEarning, domain.TaxTaxable, "BONUS", "200.0000", "GBP"),
		line("w1", domain.SourceCompBonus, domain.CategoryEarning, domain.TaxTaxable, "BONUS", "25.2500", "GBP"),
		line("w1", domain.SourceCompComponent, domain.CategoryDeduction, domain.TaxPostTax, "UNION", "10.0000", "GBP"),
		line("w1", domain.SourceBenefitEmployee, domain.CategoryBenefit, domain.TaxPreTax, "PENSION", "150.0000", "GBP"),
		line("w1", domain.SourceBenefitEmployer, domain.CategoryBenefit, domain.TaxPreTax, "PENSION-ER", "300.0000", "GBP"),
		{LineID: "q", WorkerID: "w1", Category: domain.CategoryEarning, Amount: "999999.0000", Currency: "GBP", State: domain.InputLineQuarantined},
	}
	pay, err := domain.BuildBatchPay(lines)
	require.NoError(t, err)
	p := pay["w1"]
	require.NotNil(t, p)
	assert.Equal(t, "GBP", p.Currency)
	assert.Equal(t, 3375.75, p.Gross, "base + HRA + transport + both bonuses; the quarantined line is not pay")
	assert.Equal(t, 160.0, p.Deductions, "union + the employee pension contribution; employer cost is not deducted")
	assert.Equal(t, 3175.75, p.Taxable, "taxable earnings (3000+100.5+225.25) less the pre-tax pension 150, transport excluded")

	got := map[string]domain.BatchItem{}
	for _, it := range p.Items {
		got[it.Type+":"+it.Code] = it
	}
	assert.NotContains(t, got, "EARNING:BASE", "base salary is gross less the component earnings, not an item")
	assert.Equal(t, 225.25, got["EARNING:BONUS"].Amount, "two bonus lines of one code are one payslip line")
	assert.True(t, got["EARNING:HRA"].IsTaxable)
	assert.False(t, got["EARNING:TRANSPORT"].IsTaxable)
	assert.Equal(t, 150.0, got["DEDUCTION:PENSION"].Amount)
	assert.True(t, got["DEDUCTION:PENSION"].IsTaxable == true, "a pre-tax deduction is flagged")
	assert.NotContains(t, got, "DEDUCTION:PENSION-ER")
	for i := 1; i < len(p.Items); i++ {
		a, b := p.Items[i-1], p.Items[i]
		assert.True(t, a.Type+a.Code <= b.Type+b.Code, "items come out in a fixed order")
	}
}

func TestBuildBatchPay_TaxableNeverGoesNegativeAndRoundingHappensOnce(t *testing.T) {
	lines := []domain.InputLine{
		line("w1", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "BASE", "100.0000", "GBP"),
		line("w1", domain.SourceBenefitEmployee, domain.CategoryBenefit, domain.TaxPreTax, "P", "500.0000", "GBP"),
		line("w2", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "BASE", "0.0050", "GBP"),
		line("w2", domain.SourceCompComponent, domain.CategoryEarning, domain.TaxTaxable, "A", "0.0050", "GBP"),
		line("w3", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "BASE", "0.0040", "GBP"),
	}
	pay, err := domain.BuildBatchPay(lines)
	require.NoError(t, err)
	assert.Equal(t, 0.0, pay["w1"].Taxable, "pre-tax deductions beyond taxable pay do not make it negative")
	assert.Equal(t, 0.01, pay["w2"].Gross, "0.005 + 0.005 = 0.01 exactly: rounded once, not per line")
	assert.Equal(t, 0.0, pay["w3"].Gross)
}

func TestBuildBatchPay_RefusesWhatItCannotTreat(t *testing.T) {
	for name, tc := range map[string]struct {
		lines []domain.InputLine
		want  error
	}{
		"reimbursement": {[]domain.InputLine{line("w1", domain.SourceBaseSalary, domain.CategoryReimbursement, domain.TaxNonTaxable, "X", "1.0000", "GBP")}, domain.ErrBatchCategoryUnsupported},
		"adjustment":    {[]domain.InputLine{line("w1", domain.SourceBaseSalary, domain.CategoryAdjustment, domain.TaxTaxable, "X", "1.0000", "GBP")}, domain.ErrBatchCategoryUnsupported},
		"mixed currency": {[]domain.InputLine{
			line("w1", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "B", "1.0000", "GBP"),
			line("w1", domain.SourceCompComponent, domain.CategoryEarning, domain.TaxTaxable, "A", "1.0000", "USD")}, domain.ErrBatchMixedCurrency},
	} {
		_, err := domain.BuildBatchPay(tc.lines)
		assert.ErrorIs(t, err, tc.want, name)
	}
	bad := line("w1", domain.SourceBaseSalary, domain.CategoryEarning, domain.TaxTaxable, "B", "not-money", "GBP")
	_, err := domain.BuildBatchPay([]domain.InputLine{bad})
	assert.Error(t, err)

	pay, err := domain.BuildBatchPay(nil)
	require.NoError(t, err)
	assert.Empty(t, pay)
}
