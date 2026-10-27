package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func engineLines(rule string) []domain.PaySlipTaxLine {
	mk := func(seq int, name, typ string, rate, amt float64) domain.PaySlipTaxLine {
		return domain.PaySlipTaxLine{Sequence: seq, TaxName: name, TaxType: typ, RatePct: rate, Amount: amt,
			CalculationID: "calc-1", EngineType: "STANDARD_ENGINE", RuleVersion: rule, JurisdictionCode: "GB"}
	}
	return []domain.PaySlipTaxLine{
		mk(1, "Income Tax", "STATE_FEDERAL", 15, 450),
		mk(2, "Social Insurance", "SOCIAL_SECURITY", 6.2, 186),
		mk(3, "Medical Insurance", "HEALTH_STATUTORY", 1.45, 43.5),
	}
}

func engineRun() (domain.PayrollRun, []domain.PaySlip) {
	run := domain.PayrollRun{RunID: "11111111-1111-4111-8111-111111111111", LegalEntityID: "le-1", Status: "COMPLETED", PayDate: "2026-09-30",
		EmployeeCount: 1, TotalGrossPay: 3000, TotalNetPay: 2320.50, TotalTaxDeductions: 679.50}
	hash := "snap"
	run.SnapshotHash = &hash
	slip := domain.PaySlip{EmployeeID: "22222222-2222-4222-8222-222222222222", GrossPay: 3000, TaxWithheld: 679.50, NetPay: 2320.50, TaxableAmount: 3000,
		Currency: "GBP", TaxLines: engineLines("2026.1")}
	return run, []domain.PaySlip{slip}
}

func TestTaxComponentsAreClassifiedAndTheirSumIsTheTaxWithheld(t *testing.T) {
	run, slips := engineRun()
	p := zoikoParams()
	p.SourceRunID = run.RunID
	req, err := domain.BuildZoikoResultRequest(run, slips, p)
	require.NoError(t, err)
	require.NoError(t, req.Validate())

	got := map[string]string{}
	for _, c := range req.Workers[0].Components {
		got[c.Code] = c.Kind + "=" + c.Amount
	}
	assert.Equal(t, map[string]string{
		"BASE":                  "EARNING=3000.0000",
		"TAX:INCOME_TAX":        "EMPLOYEE_TAX=450.0000",
		"TAX:SOCIAL_INSURANCE":  "EMPLOYEE_SOCIAL=186.0000",
		"TAX:MEDICAL_INSURANCE": "EMPLOYEE_SOCIAL=43.5000",
	}, got, "each engine component keeps its own class so liabilities and postings can follow it")
	assert.NotContains(t, got, "TAX_WITHHELD", "the single placeholder line is not used when the engine's lines exist")
	assert.Equal(t, "679.5000", req.Workers[0].EmployeeDeductions)
}

func TestEvidenceFlagsSayWhereTheTaxAndTheInputsCameFrom(t *testing.T) {
	run, slips := engineRun()
	p := zoikoParams()
	p.SourceRunID = run.RunID

	req, err := domain.BuildZoikoResultRequest(run, slips, p)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{domain.FlagTaxEngineUncertified, domain.FlagEmployerLiabilitiesNotComputed, domain.FlagInputNotFrozen}, req.EvidenceFlags,
		"engine tax is attributed, but an engine nobody certified is not presented as rule-backed; employer cost is never computed")

	p.TaxEngineCertified, p.InputFrozen = true, true
	req, err = domain.BuildZoikoResultRequest(run, slips, p)
	require.NoError(t, err)
	assert.Equal(t, []string{domain.FlagEmployerLiabilitiesNotComputed}, req.EvidenceFlags, "only what is genuinely still true remains")

	// A slip with tax and no engine lines is the placeholder; mixing the two keeps both labels.
	legacy := domain.PaySlip{EmployeeID: "33333333-3333-4333-8333-333333333333", GrossPay: 1000, TaxWithheld: 200, NetPay: 800, TaxableAmount: 1000, Currency: "GBP"}
	mixed := append(append([]domain.PaySlip(nil), slips...), legacy)
	run.EmployeeCount, run.TotalGrossPay, run.TotalNetPay, run.TotalTaxDeductions = 2, 4000, 3120.50, 879.50
	req, err = domain.BuildZoikoResultRequest(run, mixed, p)
	require.NoError(t, err)
	assert.Contains(t, req.EvidenceFlags, domain.FlagPlaceholderTaxRate)

	// A slip with no tax at all is neither.
	free := domain.PaySlip{EmployeeID: "44444444-4444-4444-8444-444444444444", GrossPay: 500, NetPay: 500, TaxableAmount: 0, Currency: "GBP"}
	assert.Equal(t, 0, domain.TaxSourceOf([]domain.PaySlip{free}).PlaceholderSlips)
	assert.Equal(t, 0, domain.TaxSourceOf([]domain.PaySlip{free}).EngineSlips)
}

func TestAnUnknownTaxTypeRefusesThePromotionInsteadOfBeingDefaulted(t *testing.T) {
	run, slips := engineRun()
	slips[0].TaxLines[1].TaxType = "SOMETHING_NEW"
	p := zoikoParams()
	p.SourceRunID = run.RunID
	_, err := domain.BuildZoikoResultRequest(run, slips, p)
	assert.ErrorIs(t, err, domain.ErrUnmappedTaxType)

	for _, ok := range []string{"FEDERAL", "state", "LOCAL", "STATE_FEDERAL", "SOCIAL", "social_security", "HEALTH_STATUTORY"} {
		_, err := domain.ComponentKindForTaxType(ok)
		assert.NoError(t, err, ok)
	}
	_, err = domain.ComponentKindForTaxType("")
	assert.ErrorIs(t, err, domain.ErrUnmappedTaxType)
}

func TestTaxHelpers(t *testing.T) {
	assert.Equal(t, "payroll-tax:2026.1", domain.TaxManifestVersion("2026.1"))
	assert.Equal(t, "TAX:INCOME_TAX", domain.TaxComponentCode("Income Tax"))
	assert.Equal(t, "TAX:HEALTH_INS_EE", domain.TaxComponentCode("  health (ins.) ee "))

	a := domain.TaxCorrelationID("run-1", "emp-1", 3000, "GB", "GBP")
	assert.Equal(t, a, domain.TaxCorrelationID("run-1", "emp-1", 3000, "GB", "GBP"), "identical inputs reuse the engine's stored calculation")
	assert.NotEqual(t, a, domain.TaxCorrelationID("run-1", "emp-1", 3000.01, "GB", "GBP"), "changed inputs are a new calculation")
	assert.NotEqual(t, a, domain.TaxCorrelationID("run-1", "emp-2", 3000, "GB", "GBP"))
	assert.NotEqual(t, a, domain.TaxCorrelationID("run-1", "emp-1", 3000, "US", "GBP"))

	s := domain.TaxSourceOf([]domain.PaySlip{
		{TaxLines: []domain.PaySlipTaxLine{{RuleVersion: "2026.2"}, {RuleVersion: "2026.1"}}},
		{TaxLines: []domain.PaySlipTaxLine{{RuleVersion: "2026.1"}}},
		{TaxWithheld: 10},
	})
	assert.Equal(t, 2, s.EngineSlips)
	assert.Equal(t, 1, s.PlaceholderSlips)
	assert.Equal(t, []string{"2026.1", "2026.2"}, s.RuleVersions, "distinct and sorted")
}
