package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// obligationPackInput is a pack with ONLY a calendar and an obligation rule
// (no jurisdiction rule modules): the Wave 4 modules must stand on their own.
func obligationPackInput() CompileInput {
	in := baseInput()
	in.Rules = nil
	in.Manifest.CalendarModules = []string{"11111111-1111-1111-1111-111111111111"}
	in.Manifest.ObligationModules = []string{"22222222-2222-2222-2222-222222222222"}
	in.Calendars = []CompileCalendar{{
		CalendarVersion: CalendarVersion{CalendarVersionID: "11111111-1111-1111-1111-111111111111", CalendarCode: "gb-hmrc", Version: 1,
			EffectiveFrom: d(2026, 1, 1), Timezone: "Europe/London", WeekendDays: []int{0, 6},
			Holidays: []Holiday{{"2026-11-09", "Authority closure"}}, SourceIDs: []string{"s-1"}},
		JurisdictionID: "j-gb", Published: true}}
	r := vatRule()
	r.ObligationRuleID = "22222222-2222-2222-2222-222222222222"
	r.RegimeID, r.InterpretationID, r.SourceIDs = ptr("r-vat"), ptr("i-1"), []string{"s-1"}
	r.ExtensionAllowed, r.MaxExtensionDays = true, 14
	in.Obligations = []CompileObligation{{ObligationRule: r, Published: true}}
	return in
}

func TestCompile_CalendarAndObligationModulesAloneMakeAPack(t *testing.T) {
	out := Compile(obligationPackInput())
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)
	assert.NotContains(t, codes(out.Report, SeverityWarning), "JUR-W010", "a pack with calendar and obligation modules is not empty")

	var art map[string]any
	require.NoError(t, json.Unmarshal(out.ArtifactJSON, &art))
	assert.Len(t, art["calendar_modules"], 1)
	assert.Len(t, art["obligation_modules"], 1)
	assert.Len(t, art["sources"], 1, "sources cited only by calendar and obligation modules are embedded too")
	assert.Len(t, art["interpretations"], 1)

	// Reproducible: same inputs, same digest; any change to a holiday changes it.
	assert.Equal(t, out.ArtifactDigest, Compile(obligationPackInput()).ArtifactDigest)
	in := obligationPackInput()
	in.Calendars[0].Holidays = append(in.Calendars[0].Holidays, Holiday{"2026-12-24", "Extra"})
	assert.NotEqual(t, out.ArtifactDigest, Compile(in).ArtifactDigest)

	// The artifact alone is enough to calculate a due date.
	doc, err := ParseArtifact(out.ArtifactJSON)
	require.NoError(t, err)
	res := doc.CalculateDue("GB", "VAT_RETURN", DueFacts{PeriodEnd: "2026-09-30"})
	require.Equal(t, DueCalculated, res.Outcome, "%v", res.Explanation)
	assert.Equal(t, "2026-11-10", res.DueDate, "7 Nov is a Saturday, Monday 9 Nov is a listed holiday")
	assert.Equal(t, "Europe/London", res.Timezone)
	assert.Regexp(t, `^sha256:`, res.ContentDigest)
	assert.Equal(t, DueUnsupported, doc.CalculateDue("FR", "VAT_RETURN", DueFacts{PeriodEnd: "2026-09-30"}).Outcome)
	assert.Equal(t, DueNoRule, doc.CalculateDue("GB", "OTHER", DueFacts{PeriodEnd: "2026-09-30"}).Outcome)
}

func TestCompile_RejectsEachCalendarAndObligationViolation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CompileInput)
		code   string
	}{
		{"missing calendar module", func(in *CompileInput) { in.MissingCalendarIDs = []string{"ghost"} }, "JUR-C070"},
		{"calendar repeated", func(in *CompileInput) {
			in.Manifest.CalendarModules = append(in.Manifest.CalendarModules, in.Manifest.CalendarModules[0])
		}, "JUR-C070"},
		{"calendar not published", func(in *CompileInput) { in.Calendars[0].Published = false }, "JUR-C071"},
		{"calendar outside scope", func(in *CompileInput) { in.Calendars[0].JurisdictionID = "j-other" }, "JUR-C072"},
		{"calendar malformed", func(in *CompileInput) { in.Calendars[0].Timezone = "Mars/Olympus" }, "JUR-C073"},
		{"calendar no sources", func(in *CompileInput) { in.Calendars[0].SourceIDs = nil }, "JUR-C074"},
		{"calendar unreviewed source", func(in *CompileInput) { in.Sources[0].Reviewed = false }, "JUR-C074"},
		{"missing obligation module", func(in *CompileInput) { in.MissingObligationIDs = []string{"ghost"} }, "JUR-C080"},
		{"obligation not published", func(in *CompileInput) { in.Obligations[0].Published = false }, "JUR-C081"},
		{"obligation outside scope", func(in *CompileInput) { in.Obligations[0].JurisdictionID = "j-other" }, "JUR-C082"},
		{"obligation no regime", func(in *CompileInput) { in.Obligations[0].RegimeID = nil }, "JUR-C083"},
		{"obligation regime outside pack", func(in *CompileInput) { in.Obligations[0].RegimeID = ptr("r-other") }, "JUR-C083"},
		{"obligation malformed", func(in *CompileInput) { in.Obligations[0].OffsetDays = -1 }, "JUR-C084"},
		{"obligation no sources", func(in *CompileInput) { in.Obligations[0].SourceIDs = nil }, "JUR-C085"},
		{"obligation no interpretation", func(in *CompileInput) { in.Obligations[0].InterpretationID = nil }, "JUR-C086"},
		{"obligation unapproved interpretation", func(in *CompileInput) { in.Interpretations[0].Approved = false }, "JUR-C086"},
		{"obligation calendar not in pack", func(in *CompileInput) { in.Obligations[0].CalendarCode = "de-bzst" }, "JUR-C087"},
		{"obligation overlap", func(in *CompileInput) {
			o := in.Obligations[0]
			o.ObligationRuleID, o.RuleVersion = "33333333-3333-3333-3333-333333333333", 2
			in.Obligations = append(in.Obligations, o)
		}, "JUR-C088"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := obligationPackInput()
			c.mutate(&in)
			out := Compile(in)
			assert.Contains(t, codes(out.Report, SeverityError), c.code, "%+v", out.Report.Findings)
			assert.Empty(t, out.ArtifactJSON)
		})
	}

	// Back-to-back windows of the same code are fine (a rule change, not a conflict).
	in := obligationPackInput()
	to := d(2026, 7, 1)
	in.Obligations[0].EffectiveTo = &to
	next := in.Obligations[0]
	next.ObligationRuleID, next.RuleVersion, next.EffectiveFrom, next.EffectiveTo = "33333333-3333-3333-3333-333333333333", 2, to, nil
	in.Obligations = append(in.Obligations, next)
	assert.False(t, Compile(in).Report.HasErrors())
}

func TestManifest_ValidatesCalendarAndObligationModuleIds(t *testing.T) {
	good := `{"pack_id":"jur.gb.tax.core","pack_version":"1.0","jurisdiction_ids":["GB"],"regimes":["VAT"],"effective_from":"2026-01-01",
	"calendar_modules":["11111111-1111-1111-1111-111111111111"],"obligation_modules":["22222222-2222-2222-2222-222222222222"]}`
	m, err := ParseManifest([]byte(good))
	require.NoError(t, err)
	assert.Len(t, m.CalendarModules, 1)
	_, err = ParseManifest([]byte(strings.Replace(good, "11111111-1111-1111-1111-111111111111", "not-a-uuid", 1)))
	assert.ErrorIs(t, err, ErrManifestInvalid)
	_, err = ParseManifest([]byte(strings.Replace(good, "22222222-2222-2222-2222-222222222222", "x", 1)))
	assert.ErrorIs(t, err, ErrManifestInvalid)
}

// ── harness ─────────────────────────────────────────────────────────────────

func dueCase(id, class string, in map[string]any, expect map[string]any) map[string]any {
	base := map[string]any{"jurisdiction": "GB", "obligation_code": "VAT_RETURN"}
	for k, v := range in {
		base[k] = v
	}
	return map[string]any{"id": id, "class": class, "input": base, "expect": expect}
}

func completeObligationBundle() []byte {
	rule := "22222222-2222-2222-2222-222222222222"
	return bundleOf(
		dueCase("g1", ClassGolden, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-10-07", "obligation_rule_id": rule}),
		dueCase("b1", ClassBoundary, map[string]any{"period_end": "2026-09-30"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-11-10"}),
		dueCase("n1", ClassNegative, map[string]any{"period_end": "2026-09-30", "extension_days": 15, "extension_evidence_ref": "E-1"}, map[string]any{"outcome": DueExtensionNotAllowed}),
		dueCase("n2", ClassNegative, map[string]any{"jurisdiction": "FR", "period_end": "2026-09-30"}, map[string]any{"outcome": DueUnsupported}),
		dueCase("n3", ClassNegative, map[string]any{"period_end": "2026-09-15"}, map[string]any{"outcome": DueInvalidFacts}),
	)
}

func obligationDoc(t *testing.T) *ArtifactDoc {
	t.Helper()
	out := Compile(obligationPackInput())
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)
	doc, err := ParseArtifact(out.ArtifactJSON)
	require.NoError(t, err)
	return doc
}

func TestHarness_ObligationCases_PassOnlyWithCoverage(t *testing.T) {
	b, err := ParseTestBundle(completeObligationBundle())
	require.NoError(t, err)
	r := RunTestBundle(obligationDoc(t), b)
	assert.True(t, r.Passed, "%+v %+v", r.CoverageGaps, r.Results)
	assert.Equal(t, 5, r.Total)
	assert.Equal(t, 0, r.Failed)
	assert.Equal(t, "DUE_DATE_CALCULATED:2026-11-10@22222222-2222-2222-2222-222222222222", r.Results[1].Actual)

	// A wrong expected date fails the case.
	wrong := strings.Replace(string(completeObligationBundle()), `"due_date":"2026-11-10"`, `"due_date":"2026-11-09"`, 1)
	b, err = ParseTestBundle([]byte(wrong))
	require.NoError(t, err)
	r = RunTestBundle(obligationDoc(t), b)
	assert.False(t, r.Passed)
	assert.Equal(t, 1, r.Failed)
}

func TestHarness_ObligationCoverageGapsAreSpecific(t *testing.T) {
	// Only a golden case: no adjusted boundary, no refused extension.
	only := bundleOf(
		dueCase("g1", ClassGolden, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-10-07"}),
		dueCase("n2", ClassNegative, map[string]any{"jurisdiction": "FR", "period_end": "2026-09-30"}, map[string]any{"outcome": DueUnsupported}))
	b, err := ParseTestBundle(only)
	require.NoError(t, err)
	r := RunTestBundle(obligationDoc(t), b)
	assert.False(t, r.Passed, "every case passes but coverage is incomplete")
	got := map[string]int{}
	for _, g := range r.CoverageGaps {
		got[g.Code]++
	}
	assert.Equal(t, 1, got["JUR-T011"], "the rule adjusts to a business day, so a moved-off-holiday boundary case is required (JUR-NEG-26)")
	assert.Equal(t, 1, got["JUR-T012"])
	assert.Equal(t, 0, got["JUR-T010"])

	// An unadjusted date is not a boundary case for the adjustment.
	notMoved := bundleOf(
		dueCase("g1", ClassGolden, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-10-07"}),
		dueCase("b1", ClassBoundary, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-10-07"}))
	b, _ = ParseTestBundle(notMoved)
	r = RunTestBundle(obligationDoc(t), b)
	var t011 bool
	for _, g := range r.CoverageGaps {
		t011 = t011 || g.Code == "JUR-T011"
	}
	assert.True(t, t011)
}

func TestParseTestBundle_ObligationCaseRules(t *testing.T) {
	ok := dueCase("c1", ClassGolden, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-10-07"})
	_, err := ParseTestBundle(bundleOf(ok))
	require.NoError(t, err)

	mix := dueCase("c1", ClassGolden, map[string]any{"period_end": "2026-08-31", "rule_domain": "TAX"}, map[string]any{"outcome": DueCalculated, "due_date": "2026-10-07"})
	_, err = ParseTestBundle(bundleOf(mix))
	assert.ErrorIs(t, err, ErrBundleInvalid, "an obligation case cannot also be a rule case")

	for name, c := range map[string]map[string]any{
		"calculated without date": dueCase("c1", ClassGolden, nil, map[string]any{"outcome": DueCalculated}),
		"bad due date":            dueCase("c1", ClassGolden, nil, map[string]any{"outcome": DueCalculated, "due_date": "9/9/2026"}),
		"date on a failure":       dueCase("c1", ClassNegative, nil, map[string]any{"outcome": DueInvalidFacts, "due_date": "2026-10-07"}),
		"rule outcome":            dueCase("c1", ClassGolden, nil, map[string]any{"outcome": "RESOLVED"}),
		"bad period_end":          dueCase("c1", ClassGolden, map[string]any{"period_end": "31/08/2026"}, map[string]any{"outcome": DueInvalidFacts}),
		"bad code":                dueCase("c1", ClassGolden, map[string]any{"obligation_code": "vat return"}, map[string]any{"outcome": DueNoRule}),
	} {
		_, err := ParseTestBundle(bundleOf(c))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
	}

	// Obligation fields on a rule case are rejected, so a typo cannot silently turn it into a no-op.
	ruleWithDue := map[string]any{"id": "r1", "class": ClassGolden,
		"input":  map[string]any{"jurisdiction": "GB", "rule_domain": "TAX", "rule_code": "STD", "effective_at": time.Now().UTC().Format(time.RFC3339Nano), "period_end": "2026-08-31"},
		"expect": map[string]any{"outcome": OutcomeNoRule}}
	_, err = ParseTestBundle(bundleOf(ruleWithDue))
	assert.ErrorIs(t, err, ErrBundleInvalid)
}
