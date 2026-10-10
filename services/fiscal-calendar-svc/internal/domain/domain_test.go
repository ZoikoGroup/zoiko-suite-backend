package domain_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
)

func TestParsePattern_Table(t *testing.T) {
	good445 := `{"type":"WEEK_PATTERN","weeks":[4,4,5,4,4,5,4,4,5,4,4,5],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`
	cases := []struct {
		name, in string
		ok       bool
		contains string
	}{
		{"months", `{"type":"CALENDAR_MONTHS"}`, true, ""},
		{"months with end-year label", `{"type":"CALENDAR_MONTHS","year_label":"END_YEAR"}`, true, ""},
		{"445", good445, true, ""},
		{"445 add to first", strings.Replace(good445, "ADD_TO_LAST", "ADD_TO_FIRST", 1), true, ""},
		{"months with adj", `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":true}]}`, true, ""},
		{"missing type", `{}`, false, "pattern.type is required"},
		{"unknown type", `{"type":"LUNAR"}`, false, "not supported"},
		{"unknown field", `{"type":"CALENDAR_MONTHS","typo":1}`, false, "unknown field"},
		{"not json", `nope`, false, "not valid"},
		{"months with weeks", `{"type":"CALENDAR_MONTHS","weeks":[52]}`, false, "does not take weeks"},
		{"weeks not 52", `{"type":"WEEK_PATTERN","weeks":[4,4,4],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`, false, "add up to 52"},
		{"zero week", `{"type":"WEEK_PATTERN","weeks":[0,52],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`, false, "positive"},
		{"negative week", `{"type":"WEEK_PATTERN","weeks":[-4,56],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`, false, "positive"},
		{"no weeks", `{"type":"WEEK_PATTERN","week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`, false, "pattern.weeks"},
		{"bad weekday", `{"type":"WEEK_PATTERN","weeks":[52],"week_start":"FUNDAY","extra_week_rule":"ADD_TO_LAST"}`, false, "week_start"},
		{"missing extra week rule", `{"type":"WEEK_PATTERN","weeks":[52],"week_start":"MONDAY"}`, false, "extra_week_rule"},
		{"bad year label", `{"type":"CALENDAR_MONTHS","year_label":"MID_YEAR"}`, false, "year_label"},
		{"duplicate special key", `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":true},{"key":"ADJ","position":"AFTER_LAST","zero_length":true}]}`, false, "not unique"},
		{"special key like a normal period", `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"P13","position":"AFTER_LAST","zero_length":true}]}`, false, "P<number>"},
		{"special key lowercase", `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"adj","position":"AFTER_LAST","zero_length":true}]}`, false, "key"},
		{"special position unknown", `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"ADJ","position":"MIDDLE","zero_length":true}]}`, false, "position"},
		{"special consuming days", `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":false}]}`, false, "zero_length"},
		{"too many specials", `{"type":"CALENDAR_MONTHS","special_periods":[` + strings.Repeat(`{"key":"A","position":"AFTER_LAST","zero_length":true},`, 4) + `{"key":"B","position":"AFTER_LAST","zero_length":true}]}`, false, "at most"},
		{"trailing content", `{"type":"CALENDAR_MONTHS"} {}`, false, "trailing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := domain.ParsePattern([]byte(c.in))
			if c.ok {
				require.NoError(t, err)
				assert.NotEmpty(t, p.YearLabel, "default year label is filled in")
				return
			}
			require.Error(t, err)
			de, isTyped := domain.AsError(err)
			require.True(t, isTyped)
			assert.Equal(t, domain.CodeContextInvalid, de.Code)
			assert.Contains(t, de.Message, c.contains)
		})
	}
}

func version(t *testing.T, patternJSON string, month, day int) *domain.FiscalCalendarVersion {
	t.Helper()
	p, err := domain.ParsePattern([]byte(patternJSON))
	require.NoError(t, err)
	return &domain.FiscalCalendarVersion{Pattern: *p, FiscalYearStartMonth: month, FiscalYearStartDay: day}
}

const (
	months445 = `{"type":"WEEK_PATTERN","weeks":[4,4,5,4,4,5,4,4,5,4,4,5],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`
)

func periodsJSON(t *testing.T, v *domain.FiscalCalendarVersion, fy int) string {
	t.Helper()
	ps, err := domain.PreviewPeriods(v, fy)
	require.NoError(t, err)
	b, err := json.Marshal(ps)
	require.NoError(t, err)
	return string(b)
}

func summary(ps []domain.Period) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = fmt.Sprintf("%s|%d|%s|%s|%s", p.PeriodKey, p.PeriodNo, p.StartDate, p.EndDate, p.Kind)
	}
	return out
}

func TestPreview_CalendarMonths_LeapYearFebruary(t *testing.T) {
	v := version(t, `{"type":"CALENDAR_MONTHS"}`, 1, 1)
	ps, err := domain.PreviewPeriods(v, 2024)
	require.NoError(t, err)
	require.Len(t, ps, 12)
	assert.Equal(t, "FY2024-P02|2|2024-02-01|2024-02-29|NORMAL", summary(ps)[1], "leap-year February ends on the 29th")
	ps, err = domain.PreviewPeriods(v, 2025)
	require.NoError(t, err)
	assert.Equal(t, "FY2025-P02|2|2025-02-01|2025-02-28|NORMAL", summary(ps)[1])
	assert.Equal(t, "FY2025-P12|12|2025-12-01|2025-12-31|NORMAL", summary(ps)[11])
}

func TestPreview_CalendarMonths_GoldenFullYear(t *testing.T) {
	v := version(t, `{"type":"CALENDAR_MONTHS"}`, 4, 1) // year starting 1 April, START_YEAR label
	want := `[` +
		`{"period_key":"FY2026-P01","period_no":1,"start_date":"2026-04-01","end_date":"2026-04-30","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P02","period_no":2,"start_date":"2026-05-01","end_date":"2026-05-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P03","period_no":3,"start_date":"2026-06-01","end_date":"2026-06-30","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P04","period_no":4,"start_date":"2026-07-01","end_date":"2026-07-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P05","period_no":5,"start_date":"2026-08-01","end_date":"2026-08-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P06","period_no":6,"start_date":"2026-09-01","end_date":"2026-09-30","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P07","period_no":7,"start_date":"2026-10-01","end_date":"2026-10-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P08","period_no":8,"start_date":"2026-11-01","end_date":"2026-11-30","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P09","period_no":9,"start_date":"2026-12-01","end_date":"2026-12-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P10","period_no":10,"start_date":"2027-01-01","end_date":"2027-01-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P11","period_no":11,"start_date":"2027-02-01","end_date":"2027-02-28","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P12","period_no":12,"start_date":"2027-03-01","end_date":"2027-03-31","kind":"NORMAL"}]`
	assert.Equal(t, want, periodsJSON(t, v, 2026))
}

func TestPreview_EndYearLabel(t *testing.T) {
	v := version(t, `{"type":"CALENDAR_MONTHS","year_label":"END_YEAR"}`, 4, 1)
	ps, err := domain.PreviewPeriods(v, 2027)
	require.NoError(t, err)
	assert.Equal(t, "FY2027-P01|1|2026-04-01|2026-04-30|NORMAL", summary(ps)[0], "FY2027 ends in 2027, so it starts April 2026")
	assert.Equal(t, "FY2027-P12|12|2027-03-01|2027-03-31|NORMAL", summary(ps)[11])
	// A January-1 anchor ends in the same calendar year, so START and END labels agree.
	j := version(t, `{"type":"CALENDAR_MONTHS","year_label":"END_YEAR"}`, 1, 1)
	ps, err = domain.PreviewPeriods(j, 2026)
	require.NoError(t, err)
	assert.Equal(t, "FY2026-P01|1|2026-01-01|2026-01-31|NORMAL", summary(ps)[0])
}

func TestPreview_445_52WeekYear_Golden(t *testing.T) {
	v := version(t, months445, 1, 1) // first Monday on/after 1 Jan: FY2026 = 2026-01-05 .. 2027-01-03
	want := `[` +
		`{"period_key":"FY2026-P01","period_no":1,"start_date":"2026-01-05","end_date":"2026-02-01","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P02","period_no":2,"start_date":"2026-02-02","end_date":"2026-03-01","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P03","period_no":3,"start_date":"2026-03-02","end_date":"2026-04-05","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P04","period_no":4,"start_date":"2026-04-06","end_date":"2026-05-03","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P05","period_no":5,"start_date":"2026-05-04","end_date":"2026-05-31","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P06","period_no":6,"start_date":"2026-06-01","end_date":"2026-07-05","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P07","period_no":7,"start_date":"2026-07-06","end_date":"2026-08-02","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P08","period_no":8,"start_date":"2026-08-03","end_date":"2026-08-30","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P09","period_no":9,"start_date":"2026-08-31","end_date":"2026-10-04","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P10","period_no":10,"start_date":"2026-10-05","end_date":"2026-11-01","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P11","period_no":11,"start_date":"2026-11-02","end_date":"2026-11-29","kind":"NORMAL"},` +
		`{"period_key":"FY2026-P12","period_no":12,"start_date":"2026-11-30","end_date":"2027-01-03","kind":"NORMAL"}]`
	assert.Equal(t, want, periodsJSON(t, v, 2026))
}

func TestPreview_445_53WeekYear_ExtraWeekRules(t *testing.T) {
	// FY2029 starts Mon 2029-01-01 and FY2030 starts Mon 2030-01-07: 371 days.
	last := version(t, months445, 1, 1)
	ps, err := domain.PreviewPeriods(last, 2029)
	require.NoError(t, err)
	s := summary(ps)
	assert.Equal(t, "FY2029-P01|1|2029-01-01|2029-01-28|NORMAL", s[0])
	assert.Equal(t, "FY2029-P11|11|2029-10-29|2029-11-25|NORMAL", s[10])
	assert.Equal(t, "FY2029-P12|12|2029-11-26|2030-01-06|NORMAL", s[11], "ADD_TO_LAST: the 53rd week lengthens P12 to 6 weeks")

	first := version(t, strings.Replace(months445, "ADD_TO_LAST", "ADD_TO_FIRST", 1), 1, 1)
	ps, err = domain.PreviewPeriods(first, 2029)
	require.NoError(t, err)
	s = summary(ps)
	assert.Equal(t, "FY2029-P01|1|2029-01-01|2029-02-04|NORMAL", s[0], "ADD_TO_FIRST: P01 is 5 weeks")
	assert.Equal(t, "FY2029-P12|12|2029-12-03|2030-01-06|NORMAL", s[11])

	// The neighbouring 52-week year is unaffected and starts right after.
	ps, err = domain.PreviewPeriods(last, 2030)
	require.NoError(t, err)
	assert.Equal(t, "FY2030-P01|1|2030-01-07|2030-02-03|NORMAL", summary(ps)[0])
}

func TestPreview_WithAdjustmentPeriod_Golden(t *testing.T) {
	v := version(t, `{"type":"CALENDAR_MONTHS","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":true}]}`, 1, 1)
	ps, err := domain.PreviewPeriods(v, 2026)
	require.NoError(t, err)
	require.Len(t, ps, 13)
	assert.Equal(t, "FY2026-P12|12|2026-12-01|2026-12-31|NORMAL", summary(ps)[11])
	assert.Equal(t, "FY2026-ADJ|13|2026-12-31|2026-12-31|SPECIAL", summary(ps)[12])

	w := version(t, `{"type":"WEEK_PATTERN","weeks":[4,4,5,4,4,5,4,4,5,4,4,5],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":true},{"key":"AUDIT","position":"AFTER_LAST","zero_length":true}]}`, 1, 1)
	ps, err = domain.PreviewPeriods(w, 2026)
	require.NoError(t, err)
	require.Len(t, ps, 14)
	assert.Equal(t, "FY2026-ADJ|13|2027-01-03|2027-01-03|SPECIAL", summary(ps)[12], "special period is dated at the last day of the fiscal year")
	assert.Equal(t, "FY2026-AUDIT|14|2027-01-03|2027-01-03|SPECIAL", summary(ps)[13])
}

func TestPreview_Deterministic_ByteIdentical(t *testing.T) {
	v := version(t, months445, 2, 3)
	first := periodsJSON(t, v, 2031)
	for i := 0; i < 50; i++ {
		require.Equal(t, first, periodsJSON(t, v, 2031))
	}
}

// Property: for every supported pattern and many years, the NORMAL periods are
// contiguous, non-overlapping and cover the fiscal year exactly, consecutive
// fiscal years abut, and every special period sits on the year's last day.
func TestPreview_ContiguityProperty(t *testing.T) {
	patterns := map[string]string{
		"months":        `{"type":"CALENDAR_MONTHS"}`,
		"months-end":    `{"type":"CALENDAR_MONTHS","year_label":"END_YEAR","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":true}]}`,
		"445-last":      months445,
		"445-first":     strings.Replace(months445, "ADD_TO_LAST", "ADD_TO_FIRST", 1),
		"544-sunday":    `{"type":"WEEK_PATTERN","weeks":[5,4,4,5,4,4,5,4,4,5,4,4],"week_start":"SUNDAY","extra_week_rule":"ADD_TO_LAST"}`,
		"thirteen-week": `{"type":"WEEK_PATTERN","weeks":[4,4,4,4,4,4,4,4,4,4,4,4,4],"week_start":"SATURDAY","extra_week_rule":"ADD_TO_LAST"}`,
	}
	starts := [][2]int{{1, 1}, {4, 1}, {7, 15}, {10, 28}, {12, 1}, {2, 28}}
	for name, pj := range patterns {
		for _, st := range starts {
			v := version(t, pj, st[0], st[1])
			var prevEnd domain.Date
			weekYears := 0
			for fy := 1990; fy <= 2090; fy++ {
				ps, err := domain.PreviewPeriods(v, fy)
				require.NoError(t, err, "%s start %v fy %d", name, st, fy)
				var normal []domain.Period
				for _, p := range ps {
					if p.Kind == domain.PeriodNormal {
						normal = append(normal, p)
					}
				}
				require.NotEmpty(t, normal)
				for i, p := range normal {
					assert.False(t, p.EndDate.Before(p.StartDate), "%s fy %d %s has end before start", name, fy, p.PeriodKey)
					if i > 0 {
						assert.Equal(t, normal[i-1].EndDate.AddDays(1).String(), p.StartDate.String(), "%s fy %d gap/overlap before %s", name, fy, p.PeriodKey)
					}
				}
				first, last := normal[0], normal[len(normal)-1]
				if !prevEnd.IsZero() {
					assert.Equal(t, prevEnd.AddDays(1).String(), first.StartDate.String(), "%s start %v: fy %d does not abut fy %d", name, st, fy, fy-1)
				}
				prevEnd = last.EndDate
				for i, p := range ps {
					assert.Equal(t, i+1, p.PeriodNo)
					if p.Kind == domain.PeriodSpecial {
						assert.Equal(t, last.EndDate.String(), p.StartDate.String())
						assert.Equal(t, p.StartDate.String(), p.EndDate.String())
					}
				}
				if strings.Contains(pj, "WEEK_PATTERN") {
					days := first.StartDate.DaysUntil(last.EndDate) + 1
					assert.Contains(t, []int{364, 371}, days, "%s fy %d spans %d days", name, fy, days)
					if days == 371 {
						weekYears++
					}
				}
			}
			if strings.Contains(pj, "WEEK_PATTERN") {
				assert.Greater(t, weekYears, 10, "%s start %v: 53-week years occur about every 5-6 years", name, st)
				assert.Less(t, weekYears, 30)
			}
		}
	}
}

func TestPreview_RejectsBadInput(t *testing.T) {
	v := version(t, `{"type":"CALENDAR_MONTHS"}`, 1, 1)
	_, err := domain.PreviewPeriods(v, 1800)
	assert.Error(t, err)
	_, err = domain.PreviewPeriods(v, 2500)
	assert.Error(t, err)
	_, err = domain.PreviewPeriods(nil, 2026)
	assert.Error(t, err)
	bad := version(t, `{"type":"CALENDAR_MONTHS"}`, 13, 1)
	_, err = domain.PreviewPeriods(bad, 2026)
	assert.Error(t, err)
	bad = version(t, `{"type":"CALENDAR_MONTHS"}`, 1, 29)
	_, err = domain.PreviewPeriods(bad, 2026)
	assert.Error(t, err)
}

func TestVersionStateMachine(t *testing.T) {
	legal := map[[2]domain.VersionStatus]bool{
		{domain.VersionDraft, domain.VersionApproved}:    true,
		{domain.VersionApproved, domain.VersionActive}:   true,
		{domain.VersionActive, domain.VersionSuperseded}: true,
	}
	all := []domain.VersionStatus{domain.VersionDraft, domain.VersionApproved, domain.VersionActive, domain.VersionSuperseded}
	for _, from := range all {
		for _, to := range all {
			err := domain.ValidateTransition(from, to)
			if legal[[2]domain.VersionStatus{from, to}] {
				assert.NoError(t, err, "%s -> %s", from, to)
				continue
			}
			de, ok := domain.AsError(err)
			require.True(t, ok, "%s -> %s must be refused", from, to)
			assert.Equal(t, domain.CodeInvalidTransition, de.Code)
		}
	}
	de, _ := domain.AsError(domain.ValidateTransition("BOGUS", domain.VersionActive))
	assert.Equal(t, domain.CodeInvalidTransition, de.Code)
}

func TestPlanDecision(t *testing.T) {
	assert.NoError(t, domain.ValidatePlanDecision(domain.PlanProposed, domain.PlanApproved))
	assert.NoError(t, domain.ValidatePlanDecision(domain.PlanProposed, domain.PlanRejected))
	for _, from := range []domain.PlanStatus{domain.PlanApproved, domain.PlanRejected} {
		assert.Error(t, domain.ValidatePlanDecision(from, domain.PlanApproved))
	}
	assert.Error(t, domain.ValidatePlanDecision(domain.PlanProposed, domain.PlanProposed))
}

func TestValidatePlanContent(t *testing.T) {
	imp := json.RawMessage(`{"summary":"moves year end"}`)
	_, m, err := domain.ValidatePlanContent(imp, json.RawMessage(`{"FY2026-P12":["FY2026-P12","FY2027-P01"]}`), true)
	require.NoError(t, err)
	assert.JSONEq(t, `{"FY2026-P12":["FY2026-P12","FY2027-P01"]}`, string(m))

	_, _, err = domain.ValidatePlanContent(json.RawMessage(`{}`), nil, false)
	assert.Error(t, err, "impact assessment is required")
	_, _, err = domain.ValidatePlanContent(json.RawMessage(`[]`), nil, false)
	assert.Error(t, err)
	_, _, err = domain.ValidatePlanContent(imp, json.RawMessage(`{}`), true)
	assert.Error(t, err, "posted-period impact needs a mapping")
	_, _, err = domain.ValidatePlanContent(imp, json.RawMessage(`{"a":[]}`), false)
	assert.Error(t, err)
	_, _, err = domain.ValidatePlanContent(imp, json.RawMessage(`{"a":[""]}`), false)
	assert.Error(t, err)
	_, _, err = domain.ValidatePlanContent(imp, json.RawMessage(`{"a":"b"}`), false)
	assert.Error(t, err)
	_, _, err = domain.ValidatePlanContent(imp, nil, false)
	assert.NoError(t, err, "no mapping is fine when no posted periods are affected")
}

func TestInterval(t *testing.T) {
	d := domain.NewDate
	to := d(2027, 1, 1)
	a := domain.Interval{From: d(2026, 1, 1), To: &to}
	assert.True(t, a.Contains(d(2026, 1, 1)))
	assert.True(t, a.Contains(d(2026, 12, 31)))
	assert.False(t, a.Contains(d(2027, 1, 1)), "end is exclusive")
	assert.False(t, a.Contains(d(2025, 12, 31)))
	open := domain.Interval{From: d(2027, 1, 1)}
	assert.False(t, a.Overlaps(open), "touching intervals do not overlap")
	assert.False(t, open.Overlaps(a))
	assert.True(t, open.Overlaps(domain.Interval{From: d(2030, 1, 1)}))
	assert.True(t, a.Overlaps(domain.Interval{From: d(2026, 6, 1)}))
	assert.True(t, domain.Interval{From: d(2000, 1, 1)}.Overlaps(a))
}

func TestDateJSON(t *testing.T) {
	var d domain.Date
	require.NoError(t, json.Unmarshal([]byte(`"2026-02-28"`), &d))
	assert.Equal(t, "2026-02-28", d.String())
	b, _ := json.Marshal(d)
	assert.Equal(t, `"2026-02-28"`, string(b))
	assert.Error(t, json.Unmarshal([]byte(`"2026-02-30"`), &d))
	assert.Error(t, json.Unmarshal([]byte(`"2026-02-28T00:00:00Z"`), &d))
	assert.Error(t, json.Unmarshal([]byte(`20260228`), &d))
}
