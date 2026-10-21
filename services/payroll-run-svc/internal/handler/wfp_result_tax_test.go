package handler_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
)

func engineSlip(rule string, amounts ...float64) domain.PaySlip {
	gross := 1000.0
	var lines []domain.PaySlipTaxLine
	total := 0.0
	types := []string{"STATE_FEDERAL", "SOCIAL_SECURITY", "HEALTH_STATUTORY"}
	for i, a := range amounts {
		lines = append(lines, domain.PaySlipTaxLine{Sequence: i + 1, TaxName: []string{"Income Tax", "Social Insurance", "Medical Insurance"}[i], TaxType: types[i],
			Amount: a, CalculationID: "calc", EngineType: "STANDARD_ENGINE", RuleVersion: rule, JurisdictionCode: "GB"})
		total += a
	}
	return domain.PaySlip{EmployeeID: uuid.NewString(), GrossPay: gross, TaxWithheld: total, NetPay: gross - total, TaxableAmount: gross, Currency: "GBP", TaxLines: lines}
}

func engineFixture(slip domain.PaySlip) (*stubResultStore, *domain.PayrollRun) {
	st, run := zoikoFixture()
	run.TotalTaxDeductions, run.TotalNetPay = slip.TaxWithheld, slip.NetPay
	st.slips[run.RunID] = []domain.PaySlip{slip}
	return st, run
}

func certifiedRouter(st *stubResultStore, certified bool) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPResultRoutes(r, handler.NewWFPResult(st, &stubAuthZ{}, &stubResultWorkers{}, zap.NewNop()).WithTaxEngineCertified(certified))
	return r
}

func TestWFP04_FromRunNeedsTheManifestOfTheRuleVersionTheTaxEngineUsed(t *testing.T) {
	st, run := engineFixture(engineSlip("2026.1", 100, 50, 10))

	// The default fixture manifest is not payroll-tax:2026.1.
	rr := resDo(certifiedRouter(st, false), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Equal(t, "RULE_VERSION_MANIFEST_MISMATCH", resCode(t, rr))
	assert.Empty(t, st.received, "nothing is stored against the wrong rules")

	st.manifest.ManifestVersion = domain.TaxManifestVersion("2025.9")
	rr = resDo(certifiedRouter(st, false), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, "RULE_VERSION_MANIFEST_MISMATCH", resCode(t, rr), "an older rule version's manifest is not this run's")

	st.manifest.ManifestVersion = domain.TaxManifestVersion("2026.1")
	rr = resDo(certifiedRouter(st, false), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.Len(t, st.received, 1)
	req := st.received[0].Request

	assert.NotContains(t, req.EvidenceFlags, domain.FlagPlaceholderTaxRate, "this tax is the engine's, not a placeholder")
	assert.Contains(t, req.EvidenceFlags, domain.FlagTaxEngineUncertified)
	assert.Contains(t, req.EvidenceFlags, domain.FlagEmployerLiabilitiesNotComputed, "no engine computes employer cost today")
	assert.Contains(t, req.EvidenceFlags, domain.FlagInputNotFrozen)
	kinds := map[string]string{}
	for _, c := range req.Workers[0].Components {
		kinds[c.Code] = c.Kind
	}
	assert.Equal(t, domain.KindEmployeeTax, kinds["TAX:INCOME_TAX"])
	assert.Equal(t, domain.KindEmployeeSocial, kinds["TAX:SOCIAL_INSURANCE"])
	assert.Equal(t, domain.KindEmployeeSocial, kinds["TAX:MEDICAL_INSURANCE"])
	assert.True(t, st.received[0].Evaluation.Accepted(), "%v", st.received[0].Evaluation.Reasons)
}

func TestWFP04_TheCertificationDeclarationOnlyRemovesTheUncertifiedLabel(t *testing.T) {
	st, run := engineFixture(engineSlip("2026.1", 100, 50, 10))
	st.manifest.ManifestVersion = domain.TaxManifestVersion("2026.1")
	rr := resDo(certifiedRouter(st, true), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	flags := st.received[0].Request.EvidenceFlags
	assert.NotContains(t, flags, domain.FlagTaxEngineUncertified)
	assert.Contains(t, flags, domain.FlagEmployerLiabilitiesNotComputed, "a declaration about the tax engine does not make employer cost computed")
}

func TestWFP04_ARunWhoseTaxUsedTwoRuleVersionsCannotBeOneResult(t *testing.T) {
	st, run := engineFixture(engineSlip("2026.1", 100, 50, 10))
	other := engineSlip("2026.2", 100, 50, 10)
	st.slips[run.RunID] = append(st.slips[run.RunID], other)
	run.EmployeeCount, run.TotalGrossPay, run.TotalNetPay, run.TotalTaxDeductions = 2, 2000, 1680, 320
	st.manifest.ManifestVersion = domain.TaxManifestVersion("2026.1")
	rr := resDo(certifiedRouter(st, false), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "MIXED_TAX_RULE_VERSIONS", resCode(t, rr))
	assert.Empty(t, st.received)
}

func TestWFP04_AnUnclassifiableTaxTypeRefusesThePromotion(t *testing.T) {
	slip := engineSlip("2026.1", 100, 50, 10)
	slip.TaxLines[1].TaxType = "UNHEARD_OF"
	st, run := engineFixture(slip)
	st.manifest.ManifestVersion = domain.TaxManifestVersion("2026.1")
	rr := resDo(certifiedRouter(st, false), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "TAX_TYPE_UNMAPPED", resCode(t, rr))
	assert.Empty(t, st.received)
}

func TestWFP04_ALegacyRunNeedsNoTaxManifestVersion(t *testing.T) {
	st, run := zoikoFixture() // no engine lines: the placeholder
	rr := resDo(certifiedRouter(st, false), "POST", resBase+"/from-run", fromRunBody(run), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	flags := st.received[0].Request.EvidenceFlags
	assert.Contains(t, flags, domain.FlagPlaceholderTaxRate)
	assert.NotContains(t, flags, domain.FlagTaxEngineUncertified)
}
