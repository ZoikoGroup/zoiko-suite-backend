package domain_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

const periodEnd = "2026-09-30"

func mp(kind, code, payroll, category, tax string, version int) domain.InputCodeMapping {
	return domain.InputCodeMapping{
		MappingID: "map-" + kind + "-" + code, SourceKind: kind, SourceCode: code, MappingVersion: version,
		PayrollCode: payroll, Category: category, TaxTreatment: tax, Active: true, EffectiveFrom: "2026-01-01",
	}
}

func mappings(ms ...domain.InputCodeMapping) map[string]domain.InputCodeMapping {
	out := map[string]domain.InputCodeMapping{}
	for _, m := range ms {
		out[domain.MappingKey(m.SourceKind, m.SourceCode)] = m
	}
	return out
}

var fullMappings = mappings(
	mp(domain.SourceBaseSalary, "BASE", "P-BASE", "EARNING", "TAXABLE", 1),
	mp(domain.SourceCompComponent, "HRA", "P-HRA", "EARNING", "TAXABLE", 1),
	mp(domain.SourceCompComponent, "TRAVEL", "P-TRAVEL", "EARNING", "NON_TAXABLE", 1),
	mp(domain.SourceCompComponent, "UNION", "P-UNION", "DEDUCTION", "POST_TAX", 1),
	mp(domain.SourceCompBonus, "ANNUAL", "P-BONUS", "EARNING", "TAXABLE", 1),
	mp(domain.SourceBenefitEmployee, "HEALTH_INSURANCE", "P-HEALTH-EE", "DEDUCTION", "PRE_TAX", 1),
	mp(domain.SourceBenefitEmployer, "HEALTH_INSURANCE", "P-HEALTH-ER", "BENEFIT", "NOT_APPLICABLE", 1),
)

func validWorker(id string) domain.WorkerSource {
	by := "mgr-1"
	return domain.WorkerSource{
		WorkerID: id, Context: domain.WorkerContext{Valid: true, PayCurrencies: []string{"GBP"}},
		Base:       &domain.SourceBase{ContractID: "c-1", Amount: 3000, Currency: "GBP", PayFrequency: "MONTHLY"},
		Components: []domain.SourceComponent{{StructureID: "s-1", Code: "HRA", Name: "House", Type: "EARNING", IsTaxable: true, Amount: 1200.5, Currency: "GBP"}},
		Bonuses:    []domain.SourceBonus{{GrantID: "g-1", BonusType: "ANNUAL", Amount: 500, Currency: "GBP", GrantDate: "2026-09-15", ApprovedBy: &by}},
		Elections:  []domain.SourceElection{{ElectionID: "e-1", PlanID: "pl-1", PlanType: "HEALTH_INSURANCE", TaxTreatment: "PRE_TAX", Currency: "GBP", EmployeeAmount: 50, EmployerAmount: 120}},
	}
}

func byLine(lines []domain.InputLine) map[string]domain.InputLine {
	out := map[string]domain.InputLine{}
	for _, l := range lines {
		out[l.SourceLineID] = l
	}
	return out
}

func reason(l domain.InputLine) string {
	if l.Quarantine == nil {
		return ""
	}
	return *l.Quarantine
}

func TestBuildInputLines_ClassifiesEverySourceWithProvenance(t *testing.T) {
	lines := domain.BuildInputLines([]domain.WorkerSource{validWorker("w-1")}, fullMappings, periodEnd)
	require.Len(t, lines, 5)
	m := byLine(lines)

	base := m["BASE:w-1:c-1"]
	assert.Equal(t, domain.InputLineAccepted, base.State)
	assert.Equal(t, "3000.0000", base.Amount, "exact decimal text, not a float")
	assert.Equal(t, "3000", base.SourceAmountRaw)
	assert.Equal(t, "P-BASE", *base.PayrollCode)
	assert.Equal(t, 1, *base.MappingVersion, "the mapping version used travels with the line")
	assert.Equal(t, "c-1", *base.SourceRef)
	assert.Equal(t, periodEnd, base.EffectiveDate)

	hra := m["COMP:w-1:s-1:HRA"]
	assert.Equal(t, "1200.5000", hra.Amount)
	assert.Equal(t, "1200.5", hra.SourceAmountRaw)
	assert.Equal(t, "TAXABLE", *hra.TaxTreatment)

	bonus := m["BONUS:g-1"]
	assert.Equal(t, "2026-09-15", bonus.EffectiveDate, "a bonus is effective on its grant date")
	assert.Equal(t, "mgr-1", *bonus.ApprovalRef)

	ee, er := m["BEN-EE:e-1"], m["BEN-ER:e-1"]
	assert.Equal(t, "DEDUCTION", ee.Category)
	assert.Equal(t, "BENEFIT", er.Category)
	assert.Equal(t, "50.0000", ee.Amount)
	assert.Equal(t, "120.0000", er.Amount)
	for _, l := range lines {
		assert.Len(t, l.LineHash, 64)
		assert.NotEmpty(t, l.LineID)
	}
}

func TestBuildInputLines_QuarantinesInsteadOfGuessing(t *testing.T) {
	cases := map[string]struct {
		mutate func(*domain.WorkerSource)
		maps   map[string]domain.InputCodeMapping
		line   string
		want   string
	}{
		"worker context invalid": {
			func(w *domain.WorkerSource) { w.Context = domain.WorkerContext{Reason: "NO_ACTIVE_PROFILE"} },
			fullMappings, "BASE:w-1:c-1", "NO_ACTIVE_PROFILE",
		},
		"worker invalid, no reason given": {
			func(w *domain.WorkerSource) { w.Context = domain.WorkerContext{} },
			fullMappings, "BASE:w-1:c-1", domain.InReasonWorkerContext,
		},
		"unmapped code": {
			func(w *domain.WorkerSource) { w.Components[0].Code = "MYSTERY" },
			fullMappings, "COMP:w-1:s-1:MYSTERY", domain.InReasonUnmapped,
		},
		"mapping retired (inactive)": {
			func(w *domain.WorkerSource) {},
			func() map[string]domain.InputCodeMapping {
				m := mappings(mp(domain.SourceBaseSalary, "BASE", "P-BASE", "EARNING", "TAXABLE", 2))
				x := m[domain.MappingKey(domain.SourceBaseSalary, "BASE")]
				x.Active = false
				m[domain.MappingKey(domain.SourceBaseSalary, "BASE")] = x
				return m
			}(), "BASE:w-1:c-1", domain.InReasonUnmapped,
		},
		"category conflict": {
			func(w *domain.WorkerSource) { w.Components[0].Type = "DEDUCTION" },
			fullMappings, "COMP:w-1:s-1:HRA", domain.InReasonCategoryConflict,
		},
		"taxability conflict (source taxable, mapping not)": {
			func(w *domain.WorkerSource) { w.Components[0].Code, w.Components[0].IsTaxable = "TRAVEL", true },
			fullMappings, "COMP:w-1:s-1:TRAVEL", domain.InReasonTaxabilityConflict,
		},
		"benefit tax treatment conflict": {
			func(w *domain.WorkerSource) { w.Elections[0].TaxTreatment = "POST_TAX" },
			fullMappings, "BEN-EE:e-1", domain.InReasonTaxabilityConflict,
		},
		"negative amount": {
			func(w *domain.WorkerSource) { w.Components[0].Amount = -5 },
			fullMappings, "COMP:w-1:s-1:HRA", domain.InReasonNegativeAmount,
		},
		"NaN amount": {
			func(w *domain.WorkerSource) { w.Base.Amount = math.NaN() },
			fullMappings, "BASE:w-1:c-1", domain.InReasonInvalidAmount,
		},
		"Inf amount": {
			func(w *domain.WorkerSource) { w.Components[0].Amount = math.Inf(1) },
			fullMappings, "COMP:w-1:s-1:HRA", domain.InReasonInvalidAmount,
		},
		"malformed currency": {
			func(w *domain.WorkerSource) { w.Components[0].Currency = "pounds" },
			fullMappings, "COMP:w-1:s-1:HRA", domain.InReasonInvalidCurrency,
		},
		"mixed currency is never converted": {
			func(w *domain.WorkerSource) { w.Bonuses[0].Currency = "USD" },
			fullMappings, "BONUS:g-1", domain.InReasonCurrencyMismatch,
		},
		"ambiguous pay currency": {
			func(w *domain.WorkerSource) { w.Context.PayCurrencies = []string{"GBP", "EUR"} },
			fullMappings, "BASE:w-1:c-1", domain.InReasonAmbiguousPayCurrency,
		},
		"missing provenance": {
			func(w *domain.WorkerSource) { w.Elections[0].ElectionID = "" },
			fullMappings, "BEN-EE:", domain.InReasonMissingProvenance,
		},
		"bonus without approval evidence": {
			func(w *domain.WorkerSource) { w.Bonuses[0].ApprovedBy = nil },
			fullMappings, "BONUS:g-1", domain.InReasonMissingApproval,
		},
		"bonus with blank approver": {
			func(w *domain.WorkerSource) { b := " "; w.Bonuses[0].ApprovedBy = &b },
			fullMappings, "BONUS:g-1", domain.InReasonMissingApproval,
		},
		"no active contract": {
			func(w *domain.WorkerSource) { w.Base = nil },
			fullMappings, "BASE:w-1:none", domain.InReasonNoActiveContract,
		},
	}
	for name, c := range cases {
		w := validWorker("w-1")
		c.mutate(&w)
		got := byLine(domain.BuildInputLines([]domain.WorkerSource{w}, c.maps, periodEnd))
		l, ok := got[c.line]
		require.True(t, ok, "%s: line %s missing from %v", name, c.line, got)
		assert.Equal(t, domain.InputLineQuarantined, l.State, name)
		assert.Equal(t, c.want, reason(l), name)
	}
}

func TestBuildInputLines_OneBadLineDoesNotTaintTheRest(t *testing.T) {
	w := validWorker("w-1")
	w.Components[0].Code = "MYSTERY"
	got := byLine(domain.BuildInputLines([]domain.WorkerSource{w}, fullMappings, periodEnd))
	assert.Equal(t, domain.InputLineQuarantined, got["COMP:w-1:s-1:MYSTERY"].State)
	assert.Equal(t, domain.InputLineAccepted, got["BASE:w-1:c-1"].State)
	assert.Equal(t, domain.InputLineAccepted, got["BONUS:g-1"].State)
}

func TestBuildInputLines_ZeroContributionElectionIsNotAnInput(t *testing.T) {
	w := validWorker("w-1")
	w.Elections[0].EmployeeAmount, w.Elections[0].EmployerAmount = 0, 0
	got := byLine(domain.BuildInputLines([]domain.WorkerSource{w}, fullMappings, periodEnd))
	assert.NotContains(t, got, "BEN-EE:e-1")
	assert.NotContains(t, got, "BEN-ER:e-1")
}

func TestBuildInputLines_DuplicateBonusUnderDifferentSourceIDsGoesToReview(t *testing.T) {
	w := validWorker("w-1")
	by := "mgr-2"
	w.Bonuses = append(w.Bonuses, domain.SourceBonus{GrantID: "g-0", BonusType: "ANNUAL", Amount: 500, Currency: "GBP", GrantDate: "2026-09-15", ApprovedBy: &by})
	w.Bonuses = append(w.Bonuses, domain.SourceBonus{GrantID: "g-9", BonusType: "ANNUAL", Amount: 750, Currency: "GBP", GrantDate: "2026-09-15", ApprovedBy: &by})
	got := byLine(domain.BuildInputLines([]domain.WorkerSource{w}, fullMappings, periodEnd))

	assert.Equal(t, domain.InputLineAccepted, got["BONUS:g-0"].State, "the earliest source id is kept")
	assert.Equal(t, domain.InputLineQuarantined, got["BONUS:g-1"].State)
	assert.Equal(t, domain.InReasonDuplicateCandidate, reason(got["BONUS:g-1"]))
	assert.Equal(t, domain.InputLineAccepted, got["BONUS:g-9"].State, "a different amount is a different subject")

	// Same bonus for two different workers is not a duplicate.
	two := domain.BuildInputLines([]domain.WorkerSource{validWorker("w-1"), validWorker("w-2")}, fullMappings, periodEnd)
	for _, l := range two {
		assert.Equal(t, domain.InputLineAccepted, l.State)
	}
}

func TestBuildInputLines_FrozenMappingVersionIsCarried(t *testing.T) {
	w := validWorker("w-1")
	v1 := mappings(mp(domain.SourceBaseSalary, "BASE", "OLD-CODE", "EARNING", "TAXABLE", 1))
	v2 := mappings(mp(domain.SourceBaseSalary, "BASE", "NEW-CODE", "EARNING", "TAXABLE", 2))
	l1 := byLine(domain.BuildInputLines([]domain.WorkerSource{w}, v1, periodEnd))["BASE:w-1:c-1"]
	l2 := byLine(domain.BuildInputLines([]domain.WorkerSource{w}, v2, periodEnd))["BASE:w-1:c-1"]
	assert.Equal(t, "OLD-CODE", *l1.PayrollCode)
	assert.Equal(t, 1, *l1.MappingVersion)
	assert.Equal(t, 2, *l2.MappingVersion)
	assert.NotEqual(t, l1.LineHash, l2.LineHash, "a different mapping is a different line")
}

func TestFormatAmount(t *testing.T) {
	a, b := 0.1, 0.2 // variables: a constant expression would be folded exactly
	text, raw, ok := domain.FormatAmount(a + b)
	assert.True(t, ok)
	assert.Equal(t, "0.3000", text, "float noise is rounded at the 4th decimal, and the raw source figure is kept")
	assert.Equal(t, "0.30000000000000004", raw)
	_, _, ok = domain.FormatAmount(math.NaN())
	assert.False(t, ok)
	_, _, ok = domain.FormatAmount(math.Inf(-1))
	assert.False(t, ok)
}

func TestInputSourceHash_OrderIndependentAndAcceptedOnly(t *testing.T) {
	a := domain.InputLine{SourceLineID: "a", LineHash: "h1", State: domain.InputLineAccepted}
	b := domain.InputLine{SourceLineID: "b", LineHash: "h2", State: domain.InputLineAccepted}
	q := domain.InputLine{SourceLineID: "q", LineHash: "h3", State: domain.InputLineQuarantined}
	assert.Equal(t, domain.InputSourceHash([]domain.InputLine{a, b}), domain.InputSourceHash([]domain.InputLine{b, a}))
	assert.Equal(t, domain.InputSourceHash([]domain.InputLine{a, b}), domain.InputSourceHash([]domain.InputLine{a, q, b}), "quarantined lines are outside the control hash")
	assert.NotEqual(t, domain.InputSourceHash([]domain.InputLine{a, b}), domain.InputSourceHash([]domain.InputLine{a}))
	assert.Len(t, domain.InputSourceHash(nil), 64)
}

func TestDiffInputLines(t *testing.T) {
	mk := func(id, hash, state string) domain.InputLine {
		return domain.InputLine{SourceLineID: id, LineHash: hash, State: state}
	}
	stored := []domain.InputLine{mk("a", "1", "ACCEPTED"), mk("b", "2", "ACCEPTED"), mk("c", "3", "ACCEPTED"), mk("q", "9", "QUARANTINED")}
	assert.Empty(t, domain.DiffInputLines(stored, stored))

	fresh := []domain.InputLine{mk("a", "1", "ACCEPTED"), mk("b", "CHANGED", "ACCEPTED"), mk("d", "4", "ACCEPTED"), mk("q", "9x", "QUARANTINED")}
	got := map[string]string{}
	for _, i := range domain.DiffInputLines(stored, fresh) {
		got[i.SourceLineID] = i.Issue
	}
	assert.Equal(t, map[string]string{
		"b": domain.InputIssueSourceChanged, "c": domain.InputIssueSourceMissing, "d": domain.InputIssueNewSourceLine,
	}, got, "quarantined lines are not part of the population being compared")

	// A previously quarantined line that is now valid changes the population.
	fixed := []domain.InputLine{mk("a", "1", "ACCEPTED"), mk("b", "2", "ACCEPTED"), mk("c", "3", "ACCEPTED"), mk("q", "9", "ACCEPTED")}
	issues := domain.DiffInputLines(stored, fixed)
	require.Len(t, issues, 1)
	assert.Equal(t, domain.InputIssueNewSourceLine, issues[0].Issue)
}

func TestInputRequestValidation(t *testing.T) {
	good := domain.CreateInputBatchRequest{LegalEntityID: "le-1", PayrollPeriodID: "2026-09", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30"}
	require.NoError(t, good.Validate())
	assert.Equal(t, domain.OnInvalidRejectBatch, good.WithDefaults().OnInvalid)
	for name, mutate := range map[string]func(*domain.CreateInputBatchRequest){
		"entity":     func(r *domain.CreateInputBatchRequest) { r.LegalEntityID = " " },
		"period id":  func(r *domain.CreateInputBatchRequest) { r.PayrollPeriodID = "" },
		"start":      func(r *domain.CreateInputBatchRequest) { r.PeriodStart = "x" },
		"end":        func(r *domain.CreateInputBatchRequest) { r.PeriodEnd = "2026-08-01" },
		"on_invalid": func(r *domain.CreateInputBatchRequest) { r.OnInvalid = "IGNORE" },
	} {
		r := good
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}

	m := domain.RegisterInputCodeMappingRequest{SourceKind: "COMP_BONUS", SourceCode: "ANNUAL", PayrollCode: "P", Category: "EARNING", TaxTreatment: "TAXABLE", EffectiveFrom: "2026-01-01"}
	require.NoError(t, m.Validate())
	assert.True(t, m.IsActive(), "active by default")
	off := false
	m.Active = &off
	assert.False(t, m.IsActive())
	for name, mutate := range map[string]func(*domain.RegisterInputCodeMappingRequest){
		"kind":     func(r *domain.RegisterInputCodeMappingRequest) { r.SourceKind = "PAYSLIP" },
		"code":     func(r *domain.RegisterInputCodeMappingRequest) { r.SourceCode = "" },
		"payroll":  func(r *domain.RegisterInputCodeMappingRequest) { r.PayrollCode = " " },
		"category": func(r *domain.RegisterInputCodeMappingRequest) { r.Category = "TAX" },
		"tax":      func(r *domain.RegisterInputCodeMappingRequest) { r.TaxTreatment = "MAYBE" },
		"date":     func(r *domain.RegisterInputCodeMappingRequest) { r.EffectiveFrom = "01/01/2026" },
	} {
		r := m
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
}

func TestInputEventsAreInTheCatalogue(t *testing.T) {
	for _, e := range []string{
		"payroll_input_created", "payroll_input_batch_validated", "payroll_input_batch_frozen", "payroll_input_superseded",
		"payroll_input_line_quarantined", "payroll_input_batch_rejected", "payroll_input_batch_cancelled", "payroll_input_batch_consumed",
		"payroll_input_code_mapping_registered",
	} {
		assert.True(t, domain.EventCatalogue[e], e)
	}
}
