package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// Pattern TYPES are this service's code vocabulary (how a calendar is
// computed). They are not jurisdiction values: which entity uses which pattern,
// from which start month, with which week convention, is DATA stored on a
// calendar version and validated against this vocabulary.
const (
	PatternCalendarMonths = "CALENDAR_MONTHS" // 12 months from the fiscal-year start
	PatternWeekPattern    = "WEEK_PATTERN"    // 52/53-week year split into week-counted periods (4-4-5 style)
)

// Week-pattern vocabulary.
const (
	ExtraWeekAddToLast  = "ADD_TO_LAST"  // the 53rd week lengthens the last period
	ExtraWeekAddToFirst = "ADD_TO_FIRST" // the 53rd week lengthens the first period
)

// Fiscal-year label conventions.
const (
	YearLabelStart = "START_YEAR" // FY label = year of the anchor date (default)
	YearLabelEnd   = "END_YEAR"   // FY label = year the fiscal year ends
)

// Special-period positions.
const PositionAfterLast = "AFTER_LAST"

const (
	maxPeriods        = 60
	maxSpecialPeriods = 4
	standardYearWeeks = 52
)

var weekdays = map[string]bool{"MONDAY": true, "TUESDAY": true, "WEDNESDAY": true, "THURSDAY": true, "FRIDAY": true, "SATURDAY": true, "SUNDAY": true}

var specialKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,15}$`)
var reservedKeyRe = regexp.MustCompile(`^P[0-9]+$`) // would collide with normal period keys

// SpecialPeriod is a period that consumes no calendar days (for example a 13th
// adjustment period): it is dated at the last day of the fiscal year.
type SpecialPeriod struct {
	Key        string `json:"key"`
	Position   string `json:"position"`
	ZeroLength bool   `json:"zero_length"`
}

// Pattern is the data-driven calendar definition stored as JSONB.
type Pattern struct {
	Type           string          `json:"type"`
	YearLabel      string          `json:"year_label,omitempty"`
	Weeks          []int           `json:"weeks,omitempty"`
	WeekStart      string          `json:"week_start,omitempty"`
	ExtraWeekRule  string          `json:"extra_week_rule,omitempty"`
	SpecialPeriods []SpecialPeriod `json:"special_periods,omitempty"`
}

// ParsePattern decodes and validates pattern JSON strictly: unknown fields are
// refused so a typo cannot silently change how periods are computed. The
// returned Pattern is canonical (defaults filled in).
func ParsePattern(raw []byte) (*Pattern, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Pattern
	if err := dec.Decode(&p); err != nil {
		return nil, Errf(CodeContextInvalid, "pattern is not valid: %v", err)
	}
	if dec.More() {
		return nil, Errf(CodeContextInvalid, "pattern has trailing content")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the pattern and fills the year-label default.
func (p *Pattern) Validate() error {
	switch p.YearLabel {
	case "":
		p.YearLabel = YearLabelStart
	case YearLabelStart, YearLabelEnd:
	default:
		return Errf(CodeContextInvalid, "pattern.year_label %q is not one of START_YEAR, END_YEAR", p.YearLabel)
	}

	switch p.Type {
	case PatternCalendarMonths:
		if len(p.Weeks) > 0 || p.WeekStart != "" || p.ExtraWeekRule != "" {
			return Errf(CodeContextInvalid, "pattern type CALENDAR_MONTHS does not take weeks, week_start or extra_week_rule")
		}
	case PatternWeekPattern:
		if len(p.Weeks) == 0 || len(p.Weeks) > maxPeriods {
			return Errf(CodeContextInvalid, "pattern.weeks must list between 1 and %d periods", maxPeriods)
		}
		sum := 0
		for i, w := range p.Weeks {
			if w <= 0 {
				return Errf(CodeContextInvalid, "pattern.weeks[%d] must be a positive number of weeks, got %d", i, w)
			}
			sum += w
		}
		if sum != standardYearWeeks {
			return Errf(CodeContextInvalid, "pattern.weeks must add up to %d weeks, got %d (the 53rd week is added by extra_week_rule)", standardYearWeeks, sum)
		}
		if !weekdays[p.WeekStart] {
			return Errf(CodeContextInvalid, "pattern.week_start %q must be a weekday name such as MONDAY", p.WeekStart)
		}
		if p.ExtraWeekRule != ExtraWeekAddToLast && p.ExtraWeekRule != ExtraWeekAddToFirst {
			return Errf(CodeContextInvalid, "pattern.extra_week_rule %q must be ADD_TO_LAST or ADD_TO_FIRST", p.ExtraWeekRule)
		}
	case "":
		return Errf(CodeContextInvalid, "pattern.type is required")
	default:
		return Errf(CodeContextInvalid, "pattern.type %q is not supported (supported: CALENDAR_MONTHS, WEEK_PATTERN)", p.Type)
	}

	if len(p.SpecialPeriods) > maxSpecialPeriods {
		return Errf(CodeContextInvalid, "at most %d special periods are supported", maxSpecialPeriods)
	}
	seen := map[string]bool{}
	for i, sp := range p.SpecialPeriods {
		if !specialKeyRe.MatchString(sp.Key) || reservedKeyRe.MatchString(sp.Key) {
			return Errf(CodeContextInvalid, "pattern.special_periods[%d].key %q must be 1-16 chars of A-Z, 0-9, _ starting with a letter, and not look like P<number>", i, sp.Key)
		}
		if seen[sp.Key] {
			return Errf(CodeContextInvalid, "pattern.special_periods key %q is not unique", sp.Key)
		}
		seen[sp.Key] = true
		if sp.Position != PositionAfterLast {
			return Errf(CodeContextInvalid, "pattern.special_periods[%d].position %q is not supported (supported: AFTER_LAST)", i, sp.Position)
		}
		if !sp.ZeroLength {
			return Errf(CodeContextInvalid, "pattern.special_periods[%d].zero_length must be true: special periods that consume calendar days are not supported", i)
		}
	}
	return nil
}

// CanonicalJSON is the stable JSON form stored in the database.
func (p *Pattern) CanonicalJSON() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode pattern: %w", err)
	}
	return b, nil
}
