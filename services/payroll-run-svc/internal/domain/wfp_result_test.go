package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func comp(code, kind, amount string) domain.ResultComponentInput {
	return domain.ResultComponentInput{Code: code, Kind: kind, Amount: amount}
}

// goodWorker reconciles exactly: gross 3100.50 = 3000 + 100.50; deductions 700.10 = tax 600.10 + pension 100;
// employer 250 = 250; net 3100.50 - 700.10 = 2400.40.
func goodWorker(id string) domain.ResultWorkerInput {
	return domain.ResultWorkerInput{
		WorkerID: id, Currency: "GBP", Gross: "3100.50", TaxablePay: "3000", EmployeeDeductions: "700.10",
		EmployerLiabilities: "250", NetPay: "2400.40",
		Components: []domain.ResultComponentInput{
			comp("BASE", "EARNING", "3000"), comp("HRA", "EARNING", "100.5"),
			comp("TAX", "EMPLOYEE_TAX", "600.10"), comp("PEN", "EMPLOYEE_PENSION", "100"),
			comp("NI", "EMPLOYER_SOCIAL", "250"),
		},
	}
}

func goodRequest(workers ...domain.ResultWorkerInput) domain.ReceivePayrollResultRequest {
	var gross, taxable, ded, emp, net string
	n := len(workers)
	// Two identical workers double each figure; compute exactly for the shapes used here.
	switch n {
	case 1:
		gross, taxable, ded, emp, net = "3100.50", "3000", "700.10", "250", "2400.40"
	case 2:
		gross, taxable, ded, emp, net = "6201.00", "6000", "1400.20", "500", "4800.80"
	}
	return domain.ReceivePayrollResultRequest{
		LegalEntityID: "le-1", SourceSystemID: "ext-provider", SourceRunID: "RUN-1", SourceRunVersion: 1, RunKind: "REGULAR",
		PayrollPeriodID: "2026-09", PayDate: "2026-09-30", RuleManifestID: uuid.NewString(), SourceRunStatus: "FINAL",
		SourceApprovalRef: "approval-7", CalculationRef: "engine-4.2", Currency: "GBP", Workers: workers,
		RunTotals: domain.ResultTotalsInput{WorkerCount: n, Gross: gross, TaxablePay: taxable, EmployeeDeductions: ded, EmployerLiabilities: emp, NetPay: net},
	}
}

func validCtx(ids ...string) map[string]domain.ResultWorkerContext {
	out := map[string]domain.ResultWorkerContext{}
	for _, id := range ids {
		out[id] = domain.ResultWorkerContext{Valid: true, PayCurrencies: []string{"GBP"}}
	}
	return out
}

func codes(ev domain.ResultEvaluation) []string {
	var out []string
	for _, r := range ev.Reasons {
		out = append(out, r.Code)
	}
	return out
}

func TestEvaluateResult_AReconcilingResultIsAccepted(t *testing.T) {
	w1, w2 := uuid.NewString(), uuid.NewString()
	req := goodRequest(goodWorker(w1), goodWorker(w2))
	ev := domain.EvaluateResult("t", req, validCtx(w1, w2), "")
	require.True(t, ev.Accepted(), "%v", ev.Reasons)
	assert.Equal(t, "6201.0000", ev.Totals.Gross)
	assert.Equal(t, "6000.0000", ev.Totals.TaxablePay)
	assert.Equal(t, "1400.2000", ev.Totals.EmployeeDeductions)
	assert.Equal(t, "500.0000", ev.Totals.EmployerLiabilities)
	assert.Equal(t, "4800.8000", ev.Totals.NetPay)
	assert.Equal(t, 2, ev.Totals.WorkerCount)
	require.Len(t, ev.Workers, 2)
	assert.Equal(t, "100.5000", ev.Workers[0].Components[1].Amount, "amounts are normalized to 4 decimals")
	assert.Len(t, ev.Fingerprint, 64)
}

func TestEvaluateResult_ExactnessMeansNoTolerance(t *testing.T) {
	w := uuid.NewString()
	// One cent off on net pay is a rejection, not a rounding.
	bad := goodWorker(w)
	bad.NetPay = "2400.41"
	req := goodRequest(bad)
	req.RunTotals.NetPay = "2400.41"
	ev := domain.EvaluateResult("t", req, validCtx(w), "")
	assert.Equal(t, []string{domain.RsNetPayMismatch}, codes(ev))

	// And floating-point style noise is not hidden either: 0.1 + 0.2 is exactly 0.3 here.
	x := uuid.NewString()
	nw := domain.ResultWorkerInput{
		WorkerID: x, Currency: "GBP", Gross: "0.3", TaxablePay: "0.3", EmployeeDeductions: "0", EmployerLiabilities: "0", NetPay: "0.3",
		Components: []domain.ResultComponentInput{comp("A", "EARNING", "0.1"), comp("B", "EARNING", "0.2")},
	}
	r2 := goodRequest()
	r2.Workers = []domain.ResultWorkerInput{nw}
	r2.RunTotals = domain.ResultTotalsInput{WorkerCount: 1, Gross: "0.3", TaxablePay: "0.3", EmployeeDeductions: "0", EmployerLiabilities: "0", NetPay: "0.3"}
	assert.True(t, domain.EvaluateResult("t", r2, validCtx(x), "").Accepted(), "exact decimal sums: 0.1 + 0.2 == 0.3")
}

func TestEvaluateResult_EveryControlHasItsOwnReason(t *testing.T) {
	w := uuid.NewString()
	type tc struct {
		mutate func(*domain.ReceivePayrollResultRequest)
		ctxs   map[string]domain.ResultWorkerContext
		batch  string
		want   string
	}
	cases := map[string]tc{
		"not final":                {func(r *domain.ReceivePayrollResultRequest) { r.SourceRunStatus = "CALCULATED" }, validCtx(w), "", domain.RsRunNotFinal},
		"approved is final enough": {nil, validCtx(w), "", ""},
		"missing approval":         {func(r *domain.ReceivePayrollResultRequest) { r.SourceApprovalRef = " " }, validCtx(w), "", domain.RsMissingApprovalEvidence},
		"missing calculation ref":  {func(r *domain.ReceivePayrollResultRequest) { r.CalculationRef = "" }, validCtx(w), "", domain.RsMissingCalculationRef},
		"empty result": {func(r *domain.ReceivePayrollResultRequest) {
			r.Workers = nil
			r.RunTotals = domain.ResultTotalsInput{Gross: "0", TaxablePay: "0", EmployeeDeductions: "0", EmployerLiabilities: "0", NetPay: "0"}
		}, validCtx(w), "", domain.RsEmptyResult},
		"worker unknown to WFP-01": {nil, map[string]domain.ResultWorkerContext{w: {Valid: false, Reason: "NO_ACTIVE_PROFILE"}}, "", domain.RsWorkerContext},
		"worker context missing":   {nil, map[string]domain.ResultWorkerContext{}, "", domain.RsWorkerContext},
		"pay currency mismatch":    {nil, map[string]domain.ResultWorkerContext{w: {Valid: true, PayCurrencies: []string{"EUR"}}}, "", domain.RsCurrencyMismatch},
		"ambiguous pay currency":   {nil, map[string]domain.ResultWorkerContext{w: {Valid: true, PayCurrencies: []string{"GBP", "EUR"}}}, "", domain.RsAmbiguousPayCurrency},
		"result currency vs row": {func(r *domain.ReceivePayrollResultRequest) { r.Currency = "USD" },
			map[string]domain.ResultWorkerContext{w: {Valid: true}}, "", domain.RsCurrencyMismatch},
		"gross is not earnings": {func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Components[1].Amount = "99" }, validCtx(w), "", domain.RsGrossNotEarnings},
		"deductions mismatch":   {func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Components[2].Amount = "1" }, validCtx(w), "", domain.RsDeductionsMismatch},
		"missing employer liability": {func(r *domain.ReceivePayrollResultRequest) {
			r.Workers[0].Components = r.Workers[0].Components[:4] // drop the employer social component
		}, validCtx(w), "", domain.RsEmployerMismatch},
		"net mismatch": {func(r *domain.ReceivePayrollResultRequest) {
			r.Workers[0].NetPay = "2400.39"
			r.RunTotals.NetPay = "2400.39"
		}, validCtx(w), "", domain.RsNetPayMismatch},
		"negative net": {func(r *domain.ReceivePayrollResultRequest) {
			r.Workers[0].Gross, r.Workers[0].TaxablePay, r.Workers[0].EmployeeDeductions, r.Workers[0].NetPay = "100", "100", "150", "-50"
			r.Workers[0].Components = []domain.ResultComponentInput{comp("BASE", "EARNING", "100"), comp("TAX", "EMPLOYEE_TAX", "150")}
			r.Workers[0].EmployerLiabilities = "0"
			r.RunTotals = domain.ResultTotalsInput{WorkerCount: 1, Gross: "100", TaxablePay: "100", EmployeeDeductions: "150", EmployerLiabilities: "0", NetPay: "-50"}
		}, validCtx(w), "", domain.RsNegativeNetPay},
		"declared gross total off":    {func(r *domain.ReceivePayrollResultRequest) { r.RunTotals.Gross = "3100.51" }, validCtx(w), "", domain.RsRunTotalMismatch},
		"declared worker count off":   {func(r *domain.ReceivePayrollResultRequest) { r.RunTotals.WorkerCount = 2 }, validCtx(w), "", domain.RsRunTotalMismatch},
		"declared employer total off": {func(r *domain.ReceivePayrollResultRequest) { r.RunTotals.EmployerLiabilities = "0" }, validCtx(w), "", domain.RsRunTotalMismatch},
		"input batch not frozen":      {nil, validCtx(w), "NOT_FROZEN:READY", domain.RsInputBatchInvalid},
	}
	for name, c := range cases {
		req := goodRequest(goodWorker(w))
		if name == "approved is final enough" {
			req.SourceRunStatus = "APPROVED"
		}
		if c.mutate != nil {
			c.mutate(&req)
		}
		ev := domain.EvaluateResult("t", req, c.ctxs, c.batch)
		if c.want == "" {
			assert.True(t, ev.Accepted(), "%s: %v", name, ev.Reasons)
			continue
		}
		assert.Contains(t, codes(ev), c.want, name)
		assert.False(t, ev.Accepted(), name)
	}
}

func TestEvaluateResult_DuplicateWorkerIsRejected(t *testing.T) {
	w := uuid.NewString()
	req := goodRequest(goodWorker(w), goodWorker(w))
	ev := domain.EvaluateResult("t", req, validCtx(w), "")
	assert.Contains(t, codes(ev), domain.RsDuplicateWorker)
}

func TestEvaluateResult_ReportsEveryFailureNotJustTheFirst(t *testing.T) {
	w := uuid.NewString()
	req := goodRequest(goodWorker(w))
	req.SourceRunStatus = "DRAFT"
	req.SourceApprovalRef = ""
	req.Workers[0].NetPay = "1"
	ev := domain.EvaluateResult("t", req, validCtx(w), "")
	assert.ElementsMatch(t, []string{domain.RsRunNotFinal, domain.RsMissingApprovalEvidence, domain.RsNetPayMismatch, domain.RsRunTotalMismatch}, codes(ev))
	for _, r := range ev.Reasons {
		if r.Code == domain.RsNetPayMismatch {
			assert.Equal(t, w, r.WorkerID, "a worker-level failure names the worker")
		}
	}
}

func TestResultFingerprint_StableAndSensitive(t *testing.T) {
	w1, w2 := uuid.NewString(), uuid.NewString()
	base := goodRequest(goodWorker(w1), goodWorker(w2))
	fp := domain.ResultFingerprint("t", base)

	// Order of workers and components, and the spelling of amounts, never change it.
	shuffled := goodRequest(goodWorker(w2), goodWorker(w1))
	shuffled.RuleManifestID = base.RuleManifestID
	for i := range shuffled.Workers {
		c := shuffled.Workers[i].Components
		c[0], c[len(c)-1] = c[len(c)-1], c[0]
	}
	shuffled.Workers[0].Gross = "3100.5000"
	shuffled.RunTotals.Gross = "6201"
	assert.Equal(t, fp, domain.ResultFingerprint("t", shuffled))
	assert.NotEqual(t, fp, domain.ResultFingerprint("other-tenant", base))

	mutations := map[string]func(*domain.ReceivePayrollResultRequest){
		"net":         func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].NetPay = "2400.41" },
		"component":   func(r *domain.ReceivePayrollResultRequest) { r.Workers[1].Components[0].Amount = "3001" },
		"kind":        func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Components[2].Kind = "EMPLOYEE_SOCIAL" },
		"authority":   func(r *domain.ReceivePayrollResultRequest) { a := "HMRC"; r.Workers[0].Components[4].AuthorityRef = &a },
		"status":      func(r *domain.ReceivePayrollResultRequest) { r.SourceRunStatus = "APPROVED" },
		"approval":    func(r *domain.ReceivePayrollResultRequest) { r.SourceApprovalRef = "other" },
		"calc ref":    func(r *domain.ReceivePayrollResultRequest) { r.CalculationRef = "engine-4.3" },
		"manifest":    func(r *domain.ReceivePayrollResultRequest) { r.RuleManifestID = uuid.NewString() },
		"version":     func(r *domain.ReceivePayrollResultRequest) { r.SourceRunVersion = 2 },
		"pay date":    func(r *domain.ReceivePayrollResultRequest) { r.PayDate = "2026-10-01" },
		"declared":    func(r *domain.ReceivePayrollResultRequest) { r.RunTotals.Gross = "6201.01" },
		"flags":       func(r *domain.ReceivePayrollResultRequest) { r.EvidenceFlags = []string{"X"} },
		"input batch": func(r *domain.ReceivePayrollResultRequest) { b := uuid.NewString(); r.InputBatchID = &b },
		"worker added": func(r *domain.ReceivePayrollResultRequest) {
			r.Workers = append(r.Workers, goodWorker(uuid.NewString()))
		},
	}
	for name, mutate := range mutations {
		r := goodRequest(goodWorker(w1), goodWorker(w2))
		r.RuleManifestID = base.RuleManifestID
		mutate(&r)
		assert.NotEqual(t, fp, domain.ResultFingerprint("t", r), name)
	}
}

func TestReceiveRequestValidation(t *testing.T) {
	w := uuid.NewString()
	require.NoError(t, goodRequest(goodWorker(w)).Validate())

	for name, mutate := range map[string]func(*domain.ReceivePayrollResultRequest){
		"run identity":     func(r *domain.ReceivePayrollResultRequest) { r.SourceRunID = "" },
		"currency":         func(r *domain.ReceivePayrollResultRequest) { r.Currency = "gbp" },
		"status":           func(r *domain.ReceivePayrollResultRequest) { r.SourceRunStatus = "DONE" },
		"input batch uuid": func(r *domain.ReceivePayrollResultRequest) { x := "nope"; r.InputBatchID = &x },
		"flag too long":    func(r *domain.ReceivePayrollResultRequest) { r.EvidenceFlags = []string{strings.Repeat("x", 65)} },
		"worker uuid":      func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].WorkerID = "x" },
		"worker currency":  func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Currency = "G" },
		"float-looking":    func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Gross = "3100.12345" },
		"exponent":         func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Gross = "1e3" },
		"negative gross":   func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Gross = "-1" },
		"too wide":         func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Gross = "12345678901" },
		"component kind":   func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Components[0].Kind = "BONUS" },
		"component code":   func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Components[0].Code = " " },
		"component amount": func(r *domain.ReceivePayrollResultRequest) { r.Workers[0].Components[0].Amount = "-5" },
		"run total format": func(r *domain.ReceivePayrollResultRequest) { r.RunTotals.Gross = "abc" },
		"worker count":     func(r *domain.ReceivePayrollResultRequest) { r.RunTotals.WorkerCount = -1 },
		"too many workers": func(r *domain.ReceivePayrollResultRequest) {
			r.Workers = make([]domain.ResultWorkerInput, domain.MaxResultWorkers+1)
		},
	} {
		r := goodRequest(goodWorker(w))
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}

	// A negative NET pay is representable, so it can be rejected with evidence.
	r := goodRequest(goodWorker(w))
	r.Workers[0].NetPay = "-1.50"
	assert.NoError(t, r.Validate())
	// Run totals may be as wide as the sum of many workers.
	r = goodRequest(goodWorker(w))
	r.RunTotals.Gross = "99999999999999"
	assert.NoError(t, r.Validate())
}

func TestDecimalHelpers(t *testing.T) {
	r, err := domain.ParseDecimal("12.5")
	require.NoError(t, err)
	assert.Equal(t, "12.5000", domain.FormatDecimal(r))
	for _, bad := range []string{"", "1.", ".5", "1,5", "1e3", "NaN", "0x10", "1.23456", "--1"} {
		_, err := domain.ParseDecimal(bad)
		assert.Error(t, err, bad)
	}
}

// ── Zoiko path ───────────────────────────────────────────────────────────────

func completedRun() domain.PayrollRun {
	hash := "snap-1"
	return domain.PayrollRun{
		RunID: uuid.NewString(), LegalEntityID: "le-1", Status: "COMPLETED", PayDate: "2026-09-30", EmployeeCount: 2,
		TotalGrossPay: 5100.50, TotalNetPay: 3980.40, TotalTaxDeductions: 1020.10, TotalOtherDeductions: 100,
		SnapshotHash: &hash,
	}
}

func slipsFor(run domain.PayrollRun) []domain.PaySlip {
	// worker A: flat base 3000, tax 600.10 -> net 2399.90
	// worker B: base 2000 + HRA 100.50, deduction union 100, tax 420 -> gross 2100.50, net 1580.50
	// totals: gross 5100.50, tax 1020.10, other 100, net 3980.40
	return []domain.PaySlip{
		{EmployeeID: uuid.NewString(), GrossPay: 3000, TaxWithheld: 600.10, BenefitsDeductions: 0, NetPay: 2399.90, TaxableAmount: 3000, Currency: "GBP"},
		{
			EmployeeID: uuid.NewString(), GrossPay: 2100.50, TaxWithheld: 420, BenefitsDeductions: 100, NetPay: 1580.50, TaxableAmount: 2000.50, Currency: "GBP",
			Items: []domain.PaySlipItem{
				{ComponentCode: "HRA", ComponentType: "EARNING", Amount: 100.50},
				{ComponentCode: "UNION", ComponentType: "DEDUCTION", Amount: 100},
			},
		},
	}
}

func zoikoParams() domain.ZoikoResultParams {
	return domain.ZoikoResultParams{PayrollPeriodID: "2026-09", RuleManifestID: uuid.NewString(), SourceRunVersion: 1, RunKind: "REGULAR"}
}

func TestBuildZoikoResultRequest_PromotesACompletedRunWithoutRecomputing(t *testing.T) {
	run := completedRun()
	p := zoikoParams()
	p.SourceRunID = run.RunID
	req, err := domain.BuildZoikoResultRequest(run, slipsFor(run), p)
	require.NoError(t, err)
	require.NoError(t, req.Validate())

	assert.Equal(t, domain.SourceZoikoPayroll, req.SourceSystemID)
	assert.Equal(t, "FINAL", req.SourceRunStatus)
	assert.Equal(t, "finalized:snap-1", req.SourceApprovalRef, "finality evidence is the snapshot hash taken at finalization")
	assert.Equal(t, domain.ZoikoCalculationRef, req.CalculationRef)
	assert.Equal(t, run.RunID, *req.PayrollRunID)
	assert.ElementsMatch(t, []string{domain.FlagPlaceholderTaxRate, domain.FlagEmployerLiabilitiesNotComputed, domain.FlagInputNotFrozen}, req.EvidenceFlags,
		"the calculation's known weaknesses are labelled, not implied away")
	assert.Equal(t, "5100.5000", req.RunTotals.Gross)
	assert.Equal(t, "1120.1000", req.RunTotals.EmployeeDeductions, "tax + other deductions")
	assert.Equal(t, "0.0000", req.RunTotals.EmployerLiabilities)

	// The built result reconciles exactly, so it is accepted when the rest is in order.
	ids := map[string]domain.ResultWorkerContext{}
	for _, w := range req.Workers {
		ids[w.WorkerID] = domain.ResultWorkerContext{Valid: true, PayCurrencies: []string{"GBP"}}
	}
	ev := domain.EvaluateResult("t", req, ids, "")
	assert.True(t, ev.Accepted(), "%v", ev.Reasons)
}

func TestBuildZoikoResultRequest_ApprovalEvidencePrefersAGovernanceDecision(t *testing.T) {
	run := completedRun()
	gov := "dec-9"
	run.GovernanceDecisionID = &gov
	req, err := domain.BuildZoikoResultRequest(run, slipsFor(run), zoikoParams())
	require.NoError(t, err)
	assert.Equal(t, "governance:dec-9", req.SourceApprovalRef)

	run.GovernanceDecisionID, run.SnapshotHash = nil, nil
	req, err = domain.BuildZoikoResultRequest(run, slipsFor(run), zoikoParams())
	require.NoError(t, err)
	assert.Empty(t, req.SourceApprovalRef, "no finality evidence is invented")
	ev := domain.EvaluateResult("t", req, nil, "")
	assert.Contains(t, codes(ev), domain.RsMissingApprovalEvidence)
}

func TestBuildZoikoResultRequest_RefusesRunsThatAreNotAPayrollResult(t *testing.T) {
	run := completedRun()
	run.Status = "CALCULATED"
	_, err := domain.BuildZoikoResultRequest(run, nil, zoikoParams())
	assert.ErrorIs(t, err, domain.ErrRunNotFinal)

	run = completedRun()
	run.IsShadowRun = true
	_, err = domain.BuildZoikoResultRequest(run, nil, zoikoParams())
	assert.ErrorIs(t, err, domain.ErrShadowRunNotPayable)
}

func TestBuildZoikoResultRequest_InconsistencyInTheRunIsLeftForTheControlsToReject(t *testing.T) {
	run := completedRun()
	slips := slipsFor(run)
	p := zoikoParams()

	// A payslip whose net does not equal gross - deductions.
	slips[0].NetPay = 2400.00
	req, err := domain.BuildZoikoResultRequest(run, slips, p)
	require.NoError(t, err)
	ids := map[string]domain.ResultWorkerContext{}
	for _, w := range req.Workers {
		ids[w.WorkerID] = domain.ResultWorkerContext{Valid: true, PayCurrencies: []string{"GBP"}}
	}
	ev := domain.EvaluateResult("t", req, ids, "")
	assert.Contains(t, codes(ev), domain.RsNetPayMismatch)

	// A run header whose totals do not equal its payslips.
	run2 := completedRun()
	run2.TotalGrossPay = 5100.51
	req, _ = domain.BuildZoikoResultRequest(run2, slipsFor(run2), p)
	ev = domain.EvaluateResult("t", req, ids, "")
	assert.Contains(t, codes(ev), domain.RsRunTotalMismatch)

	// Component earnings larger than gross: base is not clamped into agreement.
	slips = slipsFor(run)
	slips[1].Items[0].Amount = 9999
	req, _ = domain.BuildZoikoResultRequest(run, slips, p)
	ev = domain.EvaluateResult("t", req, ids, "")
	assert.Contains(t, codes(ev), domain.RsGrossNotEarnings)

	// Mixed currencies across payslips are caught at the worker row.
	slips = slipsFor(run)
	slips[1].Currency = "USD"
	req, _ = domain.BuildZoikoResultRequest(run, slips, p)
	ev = domain.EvaluateResult("t", req, ids, "")
	assert.Contains(t, codes(ev), domain.RsCurrencyMismatch)
}
