package domain

import (
	"fmt"
	"time"
)

// Period kinds.
const (
	PeriodNormal  = "NORMAL"
	PeriodSpecial = "SPECIAL"
)

// Supported fiscal-year labels. Wide enough for any real ledger, narrow enough
// to reject typos before they reach date arithmetic.
const (
	MinFiscalYear = 1900
	MaxFiscalYear = 2200
)

// Period is one computed accounting period of a fiscal year.
type Period struct {
	PeriodKey  string `json:"period_key"`
	FiscalYear int    `json:"-"`
	PeriodNo   int    `json:"period_no"`
	StartDate  Date   `json:"start_date"`
	EndDate    Date   `json:"end_date"`
	Kind       string `json:"kind"`
}

// ValidateStart checks the fiscal-year start anchor. Day is capped at 28 so
// month arithmetic never needs end-of-month clamping.
func ValidateStart(month, day int) error {
	if month < 1 || month > 12 {
		return Errf(CodeContextInvalid, "fiscal_year_start_month must be 1-12")
	}
	if day < 1 || day > 28 {
		return Errf(CodeContextInvalid, "fiscal_year_start_day must be 1-28")
	}
	return nil
}

var weekdayOf = map[string]time.Weekday{
	"SUNDAY": time.Sunday, "MONDAY": time.Monday, "TUESDAY": time.Tuesday, "WEDNESDAY": time.Wednesday,
	"THURSDAY": time.Thursday, "FRIDAY": time.Friday, "SATURDAY": time.Saturday,
}

// fiscalYearStart is the first day of the fiscal year whose anchor falls in
// calendar year anchorYr. For WEEK_PATTERN it is the first week_start weekday
// on or after the anchor date, which yields 52- or 53-week years and never
// drifts.
func fiscalYearStart(p *Pattern, month, day, anchorYr int) Date {
	d := NewDate(anchorYr, time.Month(month), day)
	if p.Type == PatternWeekPattern {
		want := weekdayOf[p.WeekStart]
		for d.Weekday() != want {
			d = d.AddDays(1)
		}
	}
	return d
}

// anchorYear converts a fiscal-year label to the calendar year of its anchor.
func anchorYear(p *Pattern, month, day, fy int) int {
	if p.YearLabel == YearLabelEnd && !(month == 1 && day == 1) {
		return fy - 1
	}
	return fy
}

// PreviewPeriods returns the ordered periods of fiscal year fy for a version.
// It is a pure function of (pattern, start anchor, fy): the same input always
// yields the same output. Normal periods are contiguous, non-overlapping and
// cover the whole fiscal year; special periods are zero-length and dated at
// the last day of the year.
func PreviewPeriods(v *FiscalCalendarVersion, fy int) ([]Period, error) {
	if v == nil {
		return nil, Errf(CodeContextInvalid, "version is required")
	}
	if fy < MinFiscalYear || fy > MaxFiscalYear {
		return nil, Errf(CodeContextInvalid, "fiscal_year must be between %d and %d", MinFiscalYear, MaxFiscalYear)
	}
	if err := ValidateStart(v.FiscalYearStartMonth, v.FiscalYearStartDay); err != nil {
		return nil, err
	}
	p := v.Pattern
	if err := p.Validate(); err != nil {
		return nil, err
	}

	ay := anchorYear(&p, v.FiscalYearStartMonth, v.FiscalYearStartDay, fy)
	start := fiscalYearStart(&p, v.FiscalYearStartMonth, v.FiscalYearStartDay, ay)
	next := fiscalYearStart(&p, v.FiscalYearStartMonth, v.FiscalYearStartDay, ay+1)
	lastDay := next.AddDays(-1)

	var out []Period
	add := func(key string, s, e Date, kind string) {
		out = append(out, Period{PeriodKey: key, FiscalYear: fy, PeriodNo: len(out) + 1, StartDate: s, EndDate: e, Kind: kind})
	}

	switch p.Type {
	case PatternCalendarMonths:
		for i := 0; i < 12; i++ {
			s := NewDate(ay, time.Month(v.FiscalYearStartMonth+i), v.FiscalYearStartDay)
			e := NewDate(ay, time.Month(v.FiscalYearStartMonth+i+1), v.FiscalYearStartDay).AddDays(-1)
			add(fmt.Sprintf("FY%04d-P%02d", fy, i+1), s, e, PeriodNormal)
		}
	case PatternWeekPattern:
		total := start.DaysUntil(next)
		extra := 0
		switch total {
		case standardYearWeeks * 7:
		case (standardYearWeeks + 1) * 7:
			extra = 7
		default:
			return nil, fmt.Errorf("internal: week-pattern fiscal year %d spans %d days", fy, total)
		}
		cur := start
		for i, w := range p.Weeks {
			days := w * 7
			if extra > 0 && ((p.ExtraWeekRule == ExtraWeekAddToLast && i == len(p.Weeks)-1) ||
				(p.ExtraWeekRule == ExtraWeekAddToFirst && i == 0)) {
				days += extra
			}
			e := cur.AddDays(days - 1)
			add(fmt.Sprintf("FY%04d-P%02d", fy, i+1), cur, e, PeriodNormal)
			cur = e.AddDays(1)
		}
	}
	for _, sp := range p.SpecialPeriods {
		add(fmt.Sprintf("FY%04d-%s", fy, sp.Key), lastDay, lastDay, PeriodSpecial)
	}
	return out, nil
}
