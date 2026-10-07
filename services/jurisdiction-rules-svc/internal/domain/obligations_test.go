package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }

func vatRule() ObligationRule {
	return ObligationRule{ObligationRuleID: "o1", JurisdictionID: "j-gb", ObligationCode: "VAT_RETURN", RuleVersion: 1, Name: "VAT return",
		PeriodBasis: BasisMonthly, Anchor: AnchorPeriodEnd, OffsetMonths: 1, OffsetDays: 7, OffsetToMonthEnd: true, BusinessDayAdjustment: AdjustNext, CalendarCode: "gb-hmrc",
		EffectiveFrom: d(2026, 1, 1), ContentDigest: "sha256:o1"}
}

func gbCal(version int, from time.Time, hol ...Holiday) CalendarVersion {
	return CalendarVersion{CalendarVersionID: "cv-" + string(rune('0'+version)), CalendarCode: "gb-hmrc", Version: version, EffectiveFrom: from,
		Timezone: "Europe/London", WeekendDays: []int{0, 6}, Holidays: hol}
}

func TestAddMonths_ClampsToTheEndOfAShorterMonth(t *testing.T) {
	assert.Equal(t, d(2027, 2, 28), addMonths(d(2027, 1, 31), 1))
	assert.Equal(t, d(2028, 2, 29), addMonths(d(2028, 1, 31), 1), "leap year")
	assert.Equal(t, d(2027, 1, 1), addMonths(d(2026, 11, 1), 2), "year rollover")
	assert.Equal(t, d(2026, 9, 30), addMonths(d(2026, 8, 31), 1))
	assert.Equal(t, d(2029, 3, 31), addMonths(d(2026, 3, 31), 36))
}

func TestDueDate_IsDerived_AdjustedForWeekendsAndHolidays(t *testing.T) {
	r := vatRule()
	cal := gbCal(1, d(2026, 1, 1))

	// UK-style: end of the next month (31 Oct) + 7 days = Sat 7 Nov 2026; NEXT business day is Mon 9 Nov.
	res := CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-09-30"})
	require.Equal(t, DueCalculated, res.Outcome, "%v", res.Explanation)
	assert.Equal(t, "2026-11-07", res.UnadjustedDate)
	assert.Equal(t, "2026-11-09", res.DueDate)
	assert.True(t, res.Adjusted)
	assert.Equal(t, 1, res.CalendarVersion)
	assert.Contains(t, strings.Join(res.Explanation, "|"), "weekend")

	// JUR-NEG-26: a holiday on the Monday pushes it to Tuesday.
	cal2 := gbCal(2, d(2026, 1, 1), Holiday{"2026-11-09", "Authority closure"})
	res = CalculateDueDate(r, []CalendarVersion{cal, cal2}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, "2026-11-10", res.DueDate)
	assert.Contains(t, strings.Join(res.Explanation, "|"), "Authority closure")
	assert.Equal(t, 2, res.CalendarVersion, "the amendment is a new version and wins for dates it governs")

	// PREVIOUS moves backwards; NONE leaves the date alone.
	prev := r
	prev.BusinessDayAdjustment = AdjustPrevious
	assert.Equal(t, "2026-11-06", CalculateDueDate(prev, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-09-30"}).DueDate)
	// Without offset_to_month_end the day-of-month is kept: 30 Sep + 1 month = 30 Oct, + 7 days = 6 Nov.
	sameDay := r
	sameDay.OffsetToMonthEnd = false
	sd := CalculateDueDate(sameDay, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, "2026-11-06", sd.DueDate)
	none := r
	none.BusinessDayAdjustment = AdjustNone
	nr := CalculateDueDate(none, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, "2026-11-07", nr.DueDate)
	assert.False(t, nr.Adjusted)

	// A business day is untouched.
	res = CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-08-31"}) // 7 Oct 2026 is a Wednesday
	assert.Equal(t, "2026-10-07", res.DueDate)
	assert.False(t, res.Adjusted)
}

func TestDueDate_WeekendDefinitionComesFromTheCalendar(t *testing.T) {
	r := vatRule()
	cal := gbCal(1, d(2026, 1, 1))
	cal.WeekendDays = []int{5, 6} // Friday and Saturday
	cal.Timezone = "Asia/Dubai"
	// 7 Nov 2026 is a Saturday; Sunday 8 Nov is a business day in this calendar.
	res := CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, "2026-11-08", res.DueDate)
}

func TestCalendarSelection_IsByEffectiveDate_AndAmendmentsAreNewVersions(t *testing.T) {
	r := vatRule()
	v1 := gbCal(1, d(2026, 1, 1))
	v2 := gbCal(2, d(2026, 1, 1), Holiday{"2026-11-09", "Added later"})
	v3 := gbCal(3, d(2027, 1, 1), Holiday{"2026-11-09", "Not yet in force"})
	assert.Equal(t, 2, pickCalendar([]CalendarVersion{v1, v2, v3}, "gb-hmrc", d(2026, 11, 7)).Version)
	assert.Equal(t, 3, pickCalendar([]CalendarVersion{v1, v2, v3}, "gb-hmrc", d(2027, 6, 1)).Version)
	assert.Nil(t, pickCalendar([]CalendarVersion{v1}, "gb-hmrc", d(2025, 12, 31)), "before any version is in force")
	assert.Nil(t, pickCalendar([]CalendarVersion{v1}, "other", d(2026, 6, 1)))

	// A result computed under v1 is reproducible later: only the supplied versions matter.
	old := CalculateDueDate(r, []CalendarVersion{v1}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, "2026-11-09", old.DueDate)
	assert.Equal(t, "cv-1", old.CalendarVersionID)
	now := CalculateDueDate(r, []CalendarVersion{v1, v2}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, "2026-11-10", now.DueDate, "JUR-NEG-13: only due dates governed by the amended calendar change")

	missing := CalculateDueDate(r, []CalendarVersion{v3}, DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, DueNoCalendar, missing.Outcome)
}

func TestDueDate_ExtensionsOnlyWhereLegallySupportedAndEvidenced(t *testing.T) {
	r := vatRule()
	cal := []CalendarVersion{gbCal(1, d(2026, 1, 1))}

	res := CalculateDueDate(r, cal, DueFacts{PeriodEnd: "2026-09-30", ExtensionDays: 5})
	assert.Equal(t, DueExtensionNotAllowed, res.Outcome, "this rule permits no extension")

	r.ExtensionAllowed, r.MaxExtensionDays, r.ExtensionRequiresEvidence = true, 14, true
	res = CalculateDueDate(r, cal, DueFacts{PeriodEnd: "2026-09-30", ExtensionDays: 15, ExtensionEvidence: "EXT-1"})
	assert.Equal(t, DueExtensionNotAllowed, res.Outcome, "beyond the cap")
	res = CalculateDueDate(r, cal, DueFacts{PeriodEnd: "2026-09-30", ExtensionDays: 5})
	assert.Equal(t, DueExtensionNotAllowed, res.Outcome, "no evidence")
	assert.Contains(t, res.Message, "evidenced")
	res = CalculateDueDate(r, cal, DueFacts{PeriodEnd: "2026-09-30", ExtensionDays: -1})
	assert.Equal(t, DueInvalidFacts, res.Outcome)

	// 7 Nov + 5 = Thu 12 Nov, a business day: the adjustment applies AFTER the extension.
	res = CalculateDueDate(r, cal, DueFacts{PeriodEnd: "2026-09-30", ExtensionDays: 5, ExtensionEvidence: "EXT-1"})
	require.Equal(t, DueCalculated, res.Outcome)
	assert.Equal(t, "2026-11-12", res.DueDate)
	assert.Equal(t, 5, res.ExtensionGranted)
	assert.Equal(t, "2026-11-07", res.UnadjustedDate)
}

func TestDueDate_FactsAreValidatedAgainstThePeriodBasis(t *testing.T) {
	cal := []CalendarVersion{gbCal(1, d(2026, 1, 1))}
	r := vatRule()
	assert.Equal(t, DueInvalidFacts, CalculateDueDate(r, cal, DueFacts{}).Outcome)
	assert.Contains(t, CalculateDueDate(r, cal, DueFacts{}).Message, "period_end is required")
	assert.Equal(t, DueInvalidFacts, CalculateDueDate(r, cal, DueFacts{PeriodEnd: "30/09/2026"}).Outcome)
	assert.Contains(t, CalculateDueDate(r, cal, DueFacts{PeriodEnd: "2026-09-15"}).Message, "month end", "a MONTHLY period must end on a month end")

	q := r
	q.PeriodBasis = BasisQuarterly
	assert.Equal(t, DueCalculated, CalculateDueDate(q, cal, DueFacts{PeriodEnd: "2026-09-30"}).Outcome)
	assert.Contains(t, CalculateDueDate(q, cal, DueFacts{PeriodEnd: "2026-10-31"}).Message, "quarter end")

	a := r
	a.PeriodBasis = BasisAnnual
	assert.Equal(t, DueCalculated, CalculateDueDate(a, cal, DueFacts{PeriodEnd: "2026-03-31"}).Outcome, "an annual period may end any day")

	ev := r
	ev.PeriodBasis, ev.Anchor = BasisEventDriven, AnchorEventDate
	res := CalculateDueDate(ev, cal, DueFacts{EventDate: "2026-09-10"})
	assert.Equal(t, DueCalculated, res.Outcome)
	assert.Equal(t, "2026-09-10", res.AnchorDate)
	assert.Contains(t, CalculateDueDate(ev, cal, DueFacts{PeriodEnd: "2026-09-30"}).Message, "event_date")

	reg := r
	reg.PeriodBasis, reg.Anchor = BasisEventDriven, AnchorRegistrationDate
	assert.Equal(t, DueCalculated, CalculateDueDate(reg, cal, DueFacts{RegistrationDate: "2026-02-03"}).Outcome)
}

func TestDueDate_AnniversaryAnchor_HandlesLeapDay(t *testing.T) {
	r := vatRule()
	r.PeriodBasis, r.Anchor, r.OffsetMonths, r.OffsetDays, r.BusinessDayAdjustment, r.CalendarCode = BasisAnnual, AnchorAnniversary, 0, 28, AdjustNone, ""
	res := CalculateDueDate(r, nil, DueFacts{AnniversaryDate: "2020-02-29", AnchorYear: 2027})
	require.Equal(t, DueCalculated, res.Outcome, "%v", res.Explanation)
	assert.Equal(t, "2027-02-28", res.AnchorDate, "29 February clamps in a common year")
	assert.Equal(t, "2027-03-28", res.DueDate)
	assert.Equal(t, "2028-02-29", CalculateDueDate(r, nil, DueFacts{AnniversaryDate: "2020-02-29", AnchorYear: 2028}).AnchorDate)
	assert.Equal(t, DueInvalidFacts, CalculateDueDate(r, nil, DueFacts{AnniversaryDate: "2020-02-29"}).Outcome, "anchor_year is required")
}

func TestDueDate_InstantUsesTheAuthorityTimeZoneAndCutoff(t *testing.T) {
	r := vatRule()
	r.BusinessDayAdjustment = AdjustNone
	r.OffsetMonths, r.OffsetDays = 0, 0
	cal := gbCal(1, d(2026, 1, 1))

	// End of day in London: BST (UTC+1) in August, GMT in November.
	aug := CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-08-31"})
	require.NotNil(t, aug.DueAt)
	assert.Equal(t, time.Date(2026, 8, 31, 22, 59, 59, 999999999, time.UTC), *aug.DueAt)
	assert.Equal(t, "Europe/London", aug.Timezone)
	nov := CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-11-30"})
	assert.Equal(t, time.Date(2026, 11, 30, 23, 59, 59, 999999999, time.UTC), *nov.DueAt)

	// An authority cutoff replaces end of day, but only when the rule applies it.
	cut := "17:00"
	cal.CutoffTime = &cut
	r.CutoffApplies = true
	c := CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-08-31"})
	assert.Equal(t, time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC), *c.DueAt)
	r.CutoffApplies = false
	assert.Equal(t, time.Date(2026, 8, 31, 22, 59, 59, 999999999, time.UTC), *CalculateDueDate(r, []CalendarVersion{cal}, DueFacts{PeriodEnd: "2026-08-31"}).DueAt)

	assert.False(t, c.OverdueAt(time.Date(2026, 8, 31, 15, 59, 0, 0, time.UTC)))
	assert.True(t, c.OverdueAt(time.Date(2026, 8, 31, 16, 0, 1, 0, time.UTC)))
	assert.False(t, DueResult{}.OverdueAt(time.Now()), "no due instant, never overdue")

	// No calendar at all: UTC end of day.
	r.CalendarCode = ""
	u := CalculateDueDate(r, nil, DueFacts{PeriodEnd: "2026-08-31"})
	assert.Equal(t, "UTC", u.Timezone)
}

func TestValidation_RejectsMalformedCalendarsAndRules(t *testing.T) {
	good := gbCal(1, d(2026, 1, 1), Holiday{"2026-12-25", "Christmas"})
	require.NoError(t, ValidateCalendarVersion(good))
	cut := "25:00"
	for name, mut := range map[string]func(*CalendarVersion){
		"unknown zone":   func(c *CalendarVersion) { c.Timezone = "Mars/Olympus" },
		"empty zone":     func(c *CalendarVersion) { c.Timezone = "" },
		"bad weekend":    func(c *CalendarVersion) { c.WeekendDays = []int{7} },
		"dup weekend":    func(c *CalendarVersion) { c.WeekendDays = []int{0, 0} },
		"all weekend":    func(c *CalendarVersion) { c.WeekendDays = []int{0, 1, 2, 3, 4, 5, 6} },
		"bad cutoff":     func(c *CalendarVersion) { c.CutoffTime = &cut },
		"bad holiday":    func(c *CalendarVersion) { c.Holidays = []Holiday{{"25-12-2026", "x"}} },
		"nameless":       func(c *CalendarVersion) { c.Holidays = []Holiday{{"2026-12-25", " "}} },
		"duplicate date": func(c *CalendarVersion) { c.Holidays = []Holiday{{"2026-12-25", "a"}, {"2026-12-25", "b"}} },
	} {
		c := good
		mut(&c)
		assert.ErrorIs(t, ValidateCalendarVersion(c), ErrObligationInvalid, name)
	}

	require.NoError(t, ValidateObligationRule(vatRule()))
	hrs := 0
	for name, mut := range map[string]func(*ObligationRule){
		"bad code":               func(r *ObligationRule) { r.ObligationCode = "vat return" },
		"no name":                func(r *ObligationRule) { r.Name = "" },
		"bad basis":              func(r *ObligationRule) { r.PeriodBasis = "WEEKLY" },
		"bad anchor":             func(r *ObligationRule) { r.Anchor = "TODAY" },
		"negative offset":        func(r *ObligationRule) { r.OffsetDays = -1 },
		"huge offset":            func(r *ObligationRule) { r.OffsetMonths = 61 },
		"bad adjustment":         func(r *ObligationRule) { r.BusinessDayAdjustment = "SKIP" },
		"adjustment without cal": func(r *ObligationRule) { r.CalendarCode = "" },
		"bad calendar code":      func(r *ObligationRule) { r.CalendarCode = "Bad Code" },
		"cutoff without cal": func(r *ObligationRule) {
			r.BusinessDayAdjustment, r.CalendarCode, r.CutoffApplies = AdjustNone, "", true
		},
		"extension settings w/o permission": func(r *ObligationRule) { r.MaxExtensionDays = 3 },
		"extension without cap":             func(r *ObligationRule) { r.ExtensionAllowed = true },
		"bad sla":                           func(r *ObligationRule) { r.EscalationSLAHours = &hrs },
		"inverted window":                   func(r *ObligationRule) { e := d(2025, 1, 1); r.EffectiveTo = &e },
	} {
		r := vatRule()
		mut(&r)
		assert.ErrorIs(t, ValidateObligationRule(r), ErrObligationInvalid, name)
	}
}

func TestSelectObligationRule_ByEffectiveAnchorDate_NeverGuessesOnOverlap(t *testing.T) {
	to := d(2026, 7, 1)
	v1 := vatRule()
	v1.ObligationRuleID, v1.EffectiveTo = "v1", &to
	v2 := vatRule()
	v2.ObligationRuleID, v2.RuleVersion, v2.EffectiveFrom = "v2", 2, to
	chain := map[string]bool{"j-gb": true}
	rules := []ObligationRule{v1, v2}

	pick := func(period string) (string, string) {
		r, bad := SelectObligationRule(rules, chain, "VAT_RETURN", DueFacts{PeriodEnd: period})
		if r == nil {
			return "", bad.Outcome
		}
		return r.ObligationRuleID, ""
	}
	id, _ := pick("2026-05-31")
	assert.Equal(t, "v1", id, "a period before the change keeps the old rule")
	id, _ = pick("2026-07-31")
	assert.Equal(t, "v2", id)
	_, out := pick("2025-12-31")
	assert.Equal(t, DueNoRule, out)

	_, bad := SelectObligationRule(rules, chain, "UNKNOWN_CODE", DueFacts{PeriodEnd: "2026-05-31"})
	assert.Equal(t, DueNoRule, bad.Outcome)
	_, bad = SelectObligationRule(rules, map[string]bool{"j-other": true}, "VAT_RETURN", DueFacts{PeriodEnd: "2026-05-31"})
	assert.Equal(t, DueNoRule, bad.Outcome, "a jurisdiction outside the chain has no rule")
	_, bad = SelectObligationRule(rules, chain, "VAT_RETURN", DueFacts{})
	assert.Equal(t, DueInvalidFacts, bad.Outcome)

	overlap := vatRule()
	overlap.ObligationRuleID = "dup"
	_, bad = SelectObligationRule([]ObligationRule{vatRule(), overlap}, chain, "VAT_RETURN", DueFacts{PeriodEnd: "2026-09-30"})
	assert.Equal(t, DueAmbiguous, bad.Outcome)
	assert.Contains(t, bad.Message, "dup")
}

func TestJurisdictionChain(t *testing.T) {
	scope := []ArtifactScopeEntry{{ID: "a", Code: "GB"}, {ID: "b", Code: "GB-ENG", ParentID: ptr("a")}, {ID: "c", Code: "GB-LON", ParentID: ptr("b")}}
	ch, ok := JurisdictionChain(scope, "GB-LON")
	require.True(t, ok)
	assert.Equal(t, map[string]bool{"a": true, "b": true, "c": true}, ch)
	_, ok = JurisdictionChain(scope, "FR")
	assert.False(t, ok)
}
