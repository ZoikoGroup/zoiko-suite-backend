package domain

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lc(kind, code, class, authority, from string, mods ...func(*LiabilityClass)) LiabilityClass {
	c := LiabilityClass{
		ClassRowID: "row-" + class + "-" + from, ComponentKind: kind, ComponentCode: code, ClassCode: class, LiabilityType: "TAX",
		AuthorityCounterpartyID: authority, JurisdictionID: "GB", DueRuleKind: DueDayOfFollowingMonth, DueRuleValue: 22,
		Active: true, RuleRef: "rule-1", EffectiveFrom: from,
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func comp(code, kind, amount string) PayrollResultComponent {
	return PayrollResultComponent{ComponentID: code + "-" + amount, Code: code, Kind: kind, Amount: amount}
}

func TestDueDate(t *testing.T) {
	got, err := DueDate("2026-09-30", DueDayOfFollowingMonth, 22)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-22", got)
	got, _ = DueDate("2026-12-31", DueDayOfFollowingMonth, 5)
	assert.Equal(t, "2027-01-05", got, "rolls over the year")
	got, _ = DueDate("2026-01-31", DueDaysAfterPayDate, 30)
	assert.Equal(t, "2026-03-02", got)
	got, _ = DueDate("2026-09-30", DueDaysAfterPayDate, 0)
	assert.Equal(t, "2026-09-30", got)

	for _, c := range []struct {
		kind string
		v    int
	}{{DueDayOfFollowingMonth, 0}, {DueDayOfFollowingMonth, 29}, {DueDaysAfterPayDate, -1}, {DueDaysAfterPayDate, 121}, {"WHENEVER", 5}} {
		_, err := DueDate("2026-09-30", c.kind, c.v)
		assert.Error(t, err, "%s/%d", c.kind, c.v)
	}
	_, err = DueDate("not-a-date", DueDaysAfterPayDate, 1)
	assert.Error(t, err)
}

func TestResolveClass_ExactCodeBeatsWildcardAndLatestEffectiveWins(t *testing.T) {
	classes := []LiabilityClass{
		lc(KindEmployeeTax, "*", "PAYE", "auth-1", "2026-01-01"),
		lc(KindEmployeeTax, "TAX", "PAYE-SPECIAL", "auth-2", "2026-01-01"),
		lc(KindEmployeeTax, "*", "PAYE-V2", "auth-1", "2026-09-01"),
	}
	c, ok := ResolveClass(classes, KindEmployeeTax, "TAX", "2026-09-30")
	require.True(t, ok)
	assert.Equal(t, "PAYE-SPECIAL", c.ClassCode, "the exact code wins over '*'")

	c, ok = ResolveClass(classes, KindEmployeeTax, "OTHER", "2026-09-30")
	require.True(t, ok)
	assert.Equal(t, "PAYE-V2", c.ClassCode, "latest row effective on the pay date")

	c, ok = ResolveClass(classes, KindEmployeeTax, "OTHER", "2026-08-31")
	require.True(t, ok)
	assert.Equal(t, "PAYE", c.ClassCode, "a historical run uses the rule of its own date")

	_, ok = ResolveClass(classes, KindEmployeeTax, "OTHER", "2025-12-31")
	assert.False(t, ok, "before any row is effective there is no class")
	_, ok = ResolveClass(classes, KindEmployeePension, "PEN", "2026-09-30")
	assert.False(t, ok)
}

func TestResolveClass_ARetiredRowMeansUnmappedNotAnOlderRow(t *testing.T) {
	classes := []LiabilityClass{
		lc(KindEmployeeTax, "*", "PAYE", "auth-1", "2026-01-01"),
		lc(KindEmployeeTax, "*", "PAYE", "auth-1", "2026-06-01", func(c *LiabilityClass) { c.Active = false }),
	}
	_, ok := ResolveClass(classes, KindEmployeeTax, "TAX", "2026-09-30")
	assert.False(t, ok)
	_, ok = ResolveClass(classes, KindEmployeeTax, "TAX", "2026-05-31")
	assert.True(t, ok, "still mapped before the retirement")
}

func TestPlanRecognition_GroupsByClassAuthorityAndTypeAndKeepsKindAmounts(t *testing.T) {
	classes := []LiabilityClass{
		lc(KindEmployeeTax, "*", "PAYE", "hmrc", "2026-01-01"),
		lc(KindEmployeeSocial, "*", "PAYE", "hmrc", "2026-01-01"), // same class: employee tax and NI go to one authority
		lc(KindEmployerSocial, "*", "NI-ER", "hmrc", "2026-01-01"),
		lc(KindEmployeePension, "*", "PENSION", "nest", "2026-01-01", func(c *LiabilityClass) {
			c.LiabilityType, c.DueRuleKind, c.DueRuleValue = "PENSION", DueDaysAfterPayDate, 10
		}),
	}
	ref := "HMRC-REF"
	comps := []PayrollResultComponent{
		{ComponentID: "1", Code: "TAX", Kind: KindEmployeeTax, Amount: "600.10", AuthorityRef: &ref},
		{ComponentID: "2", Code: "TAX", Kind: KindEmployeeTax, Amount: "400.05"},
		comp("NI", KindEmployeeSocial, "100"),
		comp("NI", KindEmployerSocial, "250"),
		comp("PEN", KindEmployeePension, "100"),
		comp("BASE", KindEarning, "3000"),
	}
	plan, err := PlanRecognition(PlanInput{PayDate: "2026-09-30", Components: comps, Classes: classes})
	require.NoError(t, err)
	assert.Empty(t, plan.Exceptions)
	require.Len(t, plan.Liabilities, 3)

	byClass := map[string]LiabilityDraft{}
	for _, d := range plan.Liabilities {
		byClass[d.Class.ClassCode] = d
	}
	paye := byClass["PAYE"]
	assert.Equal(t, "1100.1500", FormatDecimal(paye.Amount))
	assert.Equal(t, []string{KindEmployeeSocial, KindEmployeeTax}, paye.ComponentKinds)
	assert.Equal(t, "1000.1500", FormatDecimal(paye.KindAmounts[KindEmployeeTax]))
	assert.Equal(t, "100.0000", FormatDecimal(paye.KindAmounts[KindEmployeeSocial]))
	assert.Equal(t, SideEmployeeWithheld, paye.PartySide)
	assert.Equal(t, "2026-10-22", paye.DueDate)
	assert.Equal(t, 3, paye.SourceCount)
	assert.Equal(t, []string{"HMRC-REF"}, paye.AuthorityLabels)

	assert.Equal(t, SideEmployer, byClass["NI-ER"].PartySide)
	assert.Equal(t, "250.0000", FormatDecimal(byClass["NI-ER"].Amount))
	assert.Equal(t, "2026-10-10", byClass["PENSION"].DueDate, "its own due rule")
}

func TestPlanRecognition_AnUncoveredComponentIsAnExceptionNeverAGuess(t *testing.T) {
	classes := []LiabilityClass{lc(KindEmployeeTax, "*", "PAYE", "hmrc", "2026-01-01")}
	plan, err := PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: classes, Components: []PayrollResultComponent{
		comp("TAX", KindEmployeeTax, "600"), comp("PEN", KindEmployeePension, "100"), comp("PEN", KindEmployeePension, "50.5"),
		comp("BASE", KindEarning, "3000"),
	}})
	require.NoError(t, err)
	require.Len(t, plan.Liabilities, 1)
	require.Len(t, plan.Exceptions, 1)
	e := plan.Exceptions[0]
	assert.Equal(t, ExcUnmappedClass, e.Code)
	assert.Equal(t, "EMPLOYEE_PENSION:PEN", e.SubjectKey)
	require.NotNil(t, e.Amount)
	assert.Equal(t, "150.5000", *e.Amount, "the unrecognized money is visible")
}

func TestPlanRecognition_DisagreeingRowsOfOneClassAreAConflict(t *testing.T) {
	classes := []LiabilityClass{
		lc(KindEmployeeTax, "*", "PAYE", "hmrc", "2026-01-01"),
		lc(KindEmployeeSocial, "*", "PAYE", "hmrc", "2026-01-01", func(c *LiabilityClass) { c.DueRuleValue = 19 }),
	}
	plan, err := PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: classes, Components: []PayrollResultComponent{
		comp("TAX", KindEmployeeTax, "600"), comp("NI", KindEmployeeSocial, "100"),
	}})
	require.NoError(t, err)
	assert.Empty(t, plan.Liabilities, "no liability is guessed from a contradictory registry")
	require.Len(t, plan.Exceptions, 1)
	assert.Equal(t, ExcClassConflict, plan.Exceptions[0].Code)
}

func TestPlanRecognition_CompletenessAndTheZoikoEmployerFlag(t *testing.T) {
	classes := []LiabilityClass{
		lc(KindEmployeeTax, "*", "PAYE", "hmrc", "2026-01-01"),
		lc(KindEmployerSocial, "*", "NI-ER", "hmrc", "2026-01-01", func(c *LiabilityClass) { c.Required = true }),
		lc(KindEmployerPension, "PEN-ER", "PEN-ER", "nest", "2026-01-01", func(c *LiabilityClass) { c.Required = true }),
	}
	plan, err := PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: classes,
		Flags:      []string{FlagPlaceholderTaxRate, FlagEmployerLiabilitiesNotComputed},
		Components: []PayrollResultComponent{comp("TAX", KindEmployeeTax, "600")}})
	require.NoError(t, err)
	var codes []string
	for _, e := range plan.Exceptions {
		codes = append(codes, e.Code+"/"+e.SubjectKey)
	}
	assert.Equal(t, []string{
		ExcEmployerNotComputed + "/RESULT",
		ExcMissingExpected + "/EMPLOYER_PENSION:PEN-ER",
		ExcMissingExpected + "/EMPLOYER_SOCIAL:*",
	}, codes)

	// Present (even at zero) satisfies the requirement.
	plan, err = PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: classes, Components: []PayrollResultComponent{
		comp("TAX", KindEmployeeTax, "600"), comp("NI", KindEmployerSocial, "0"), comp("PEN-ER", KindEmployerPension, "10"),
	}})
	require.NoError(t, err)
	assert.Empty(t, plan.Exceptions)
	require.Len(t, plan.Liabilities, 2, "the zero-amount class creates no liability")

	// A required row that is not yet effective on the pay date does not apply.
	later := []LiabilityClass{lc(KindEmployerSocial, "*", "NI-ER", "hmrc", "2026-10-01", func(c *LiabilityClass) { c.Required = true })}
	plan, _ = PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: later})
	assert.Empty(t, plan.Exceptions)
}

func TestPlanRecognition_IsDeterministic(t *testing.T) {
	classes := []LiabilityClass{
		lc(KindEmployeeTax, "*", "A", "x", "2026-01-01"), lc(KindEmployeeSocial, "*", "B", "y", "2026-01-01"),
		lc(KindEmployerTax, "*", "C", "z", "2026-01-01"),
	}
	comps := []PayrollResultComponent{comp("1", KindEmployerTax, "3"), comp("2", KindEmployeeSocial, "2"), comp("3", KindEmployeeTax, "1")}
	a, err := PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: classes, Components: comps})
	require.NoError(t, err)
	b, err := PlanRecognition(PlanInput{PayDate: "2026-09-30", Classes: classes, Components: comps})
	require.NoError(t, err)
	require.Len(t, a.Liabilities, 3)
	for i := range a.Liabilities {
		assert.Equal(t, a.Liabilities[i].Class.ClassCode, b.Liabilities[i].Class.ClassCode)
	}
	assert.Equal(t, "A", a.Liabilities[0].Class.ClassCode)
}

func TestApplySettlement(t *testing.T) {
	r := func(s string) *big.Rat { v, _ := new(big.Rat).SetString(s); return v }
	settled, state, err := ApplySettlement(r("100"), r("0"), r("40"))
	require.NoError(t, err)
	assert.Equal(t, LiabPartiallySettled, state)
	assert.Equal(t, "40.0000", FormatDecimal(settled))

	settled, state, err = ApplySettlement(r("100"), r("40"), r("60"))
	require.NoError(t, err)
	assert.Equal(t, LiabSettled, state)
	assert.Equal(t, "100.0000", FormatDecimal(settled))

	_, _, err = ApplySettlement(r("100"), r("40"), r("60.0001"))
	assert.ErrorIs(t, err, ErrOverSettlement)

	// Exact arithmetic: 0.1 + 0.2 settles 0.3 precisely.
	_, state, err = ApplySettlement(r("0.3"), r("0.1"), r("0.2"))
	require.NoError(t, err)
	assert.Equal(t, LiabSettled, state)
}

func TestReconcileKeys_ComparesBothSides(t *testing.T) {
	r := func(s string) *big.Rat { v, _ := new(big.Rat).SetString(s); return v }
	lines := ReconcileKeys(
		map[string]*big.Rat{"A": r("10"), "B": r("5")},
		map[string]*big.Rat{"A": r("10"), "C": r("7")})
	require.Len(t, lines, 3)
	assert.Equal(t, ReconcileLine{MappingKey: "A", Liabilities: "10.0000", Posted: "10.0000", Matches: true}, lines[0])
	assert.False(t, lines[1].Matches, "a liability with no posted credit differs")
	assert.False(t, lines[2].Matches, "a posted payable with no liability differs")
}

func TestPayableKeyOfMatchesTheWave5Keys(t *testing.T) {
	assert.Equal(t, "PAYROLL_EMPLOYEE_TAX_PAYABLE", PayableKeyOf(KindEmployeeTax))
	assert.Equal(t, "PAYROLL_EMPLOYEE_DEDUCTION_PAYABLE", PayableKeyOf(KindEmployeeDeduction))
	assert.Equal(t, "PAYROLL_EMPLOYER_SOCIAL_PAYABLE", PayableKeyOf(KindEmployerSocial))
	all := map[string]bool{}
	for _, k := range AllPayrollMappingKeys() {
		all[k] = true
	}
	for _, kind := range []string{KindEmployeeDeduction, KindEmployeeTax, KindEmployeeSocial, KindEmployeePension, KindEmployeeBenefit,
		KindEmployerTax, KindEmployerSocial, KindEmployerPension, KindEmployerBenefit, KindEmployerOther} {
		assert.True(t, all[PayableKeyOf(kind)], "%s must map to a key WFP-05 actually posts", kind)
	}
}

func TestRegisterLiabilityClassValidation(t *testing.T) {
	ok := RegisterLiabilityClassRequest{
		LegalEntityID: "le-1", ComponentKind: KindEmployeeTax, ClassCode: "PAYE", LiabilityType: "TAX", AuthorityCounterpartyID: "cp-1",
		JurisdictionID: "GB", DueRuleKind: DueDayOfFollowingMonth, DueRuleValue: 22, RuleRef: "gov-rule-1", EffectiveFrom: "2026-01-01",
	}
	r := ok
	require.NoError(t, r.Validate())
	assert.Equal(t, "*", r.ComponentCode, "no code means any code of the kind")

	for name, mutate := range map[string]func(*RegisterLiabilityClassRequest){
		"earning":      func(r *RegisterLiabilityClassRequest) { r.ComponentKind = KindEarning },
		"bad type":     func(r *RegisterLiabilityClassRequest) { r.LiabilityType = "GIFT" },
		"no authority": func(r *RegisterLiabilityClassRequest) { r.AuthorityCounterpartyID = " " },
		"no rule ref":  func(r *RegisterLiabilityClassRequest) { r.RuleRef = "" },
		"bad due":      func(r *RegisterLiabilityClassRequest) { r.DueRuleValue = 31 },
		"bad date":     func(r *RegisterLiabilityClassRequest) { r.EffectiveFrom = "01/01/2026" },
		"bad code":     func(r *RegisterLiabilityClassRequest) { r.ComponentCode = "a b" },
	} {
		r := ok
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
}

func TestSettleAndAdjustValidation(t *testing.T) {
	assert.NoError(t, SettleLiabilityRequest{PaymentID: "p-1", Amount: "10.5", Currency: "GBP"}.Validate())
	for _, r := range []SettleLiabilityRequest{
		{PaymentID: "", Amount: "10", Currency: "GBP"}, {PaymentID: "p", Amount: "0", Currency: "GBP"},
		{PaymentID: "p", Amount: "-1", Currency: "GBP"}, {PaymentID: "p", Amount: "10", Currency: "gbp"},
		{PaymentID: "p", Amount: "10.12345", Currency: "GBP"},
	} {
		assert.Error(t, r.Validate(), "%+v", r)
	}
	assert.NoError(t, AdjustLiabilityRequest{Delta: "-5", Reason: "overstated", EvidenceRef: "doc-1"}.Validate())
	assert.Error(t, AdjustLiabilityRequest{Delta: "0", Reason: "r", EvidenceRef: "e"}.Validate())
	assert.Error(t, AdjustLiabilityRequest{Delta: "1", Reason: "", EvidenceRef: "e"}.Validate())
}

func TestOutstanding(t *testing.T) {
	o, err := Outstanding("100.0000", "-10.0000", "30.0000")
	require.NoError(t, err)
	assert.Equal(t, "60.0000", o)
}
