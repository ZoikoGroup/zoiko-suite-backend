package domain

// REF-10 Business Calendar (ZS-SVC-I-001 s4.20): holidays and business-day
// conventions, with deterministic business-day arithmetic.
//
// "Business-day calculation deterministic": nothing here reads a clock or
// guesses. A date is a business day or it is not, by the calendar version it is
// asked of, and every answer says which version that was ("applied
// term/calendar provenance returned"), so it can be reproduced later.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Observed-holiday rules: how a holiday that falls on a weekend is observed.
// A holiday on a weekday is observed on itself under every rule.
const (
	ObservedNone            = "NONE"
	ObservedNextWeekday     = "WEEKEND_TO_NEXT_WEEKDAY"
	ObservedPreviousWeekday = "WEEKEND_TO_PREVIOUS_WEEKDAY"
	// ObservedNearestWeekday moves to the nearest weekday, the following one on
	// a tie: Saturday to Friday and Sunday to Monday for a Saturday-Sunday weekend.
	ObservedNearestWeekday = "WEEKEND_TO_NEAREST_WEEKDAY"
)

// ValidObservedRule reports whether s names an observed-holiday rule.
func ValidObservedRule(s string) bool {
	switch s {
	case ObservedNone, ObservedNextWeekday, ObservedPreviousWeekday, ObservedNearestWeekday:
		return true
	}
	return false
}

// Business-day conventions: what to do with a date that is not a business day.
const (
	ConventionNone              = "NONE"
	ConventionFollowing         = "FOLLOWING"
	ConventionPreceding         = "PRECEDING"
	ConventionModifiedFollowing = "MODIFIED_FOLLOWING"
	ConventionModifiedPreceding = "MODIFIED_PRECEDING"
)

// ValidConvention reports whether s names a business-day convention.
func ValidConvention(s string) bool {
	switch s {
	case ConventionNone, ConventionFollowing, ConventionPreceding, ConventionModifiedFollowing, ConventionModifiedPreceding:
		return true
	}
	return false
}

// Why a day is not a business day.
const (
	ReasonWeekend = "WEEKEND"
	ReasonHoliday = "HOLIDAY"
)

// Bounds on arithmetic: a shift larger than this is a mistake, and a calendar
// with this many non-business days in a row has none.
const (
	MaxBusinessDayShift       = 1000
	maxConsecutiveNonBusiness = 366
)

// SkippedDay is a day a calculation passed over, and why.
type SkippedDay struct {
	Date   string `json:"date"`
	Reason string `json:"reason"`
	Name   string `json:"name,omitempty"`
}

// ComputeObservedDate is the date a holiday on `date` is observed under rule,
// for a calendar with the given weekend (0 = Sunday ... 6 = Saturday). A holiday
// on a weekday, and any holiday under NONE, is observed on its own date.
func ComputeObservedDate(date time.Time, rule string, weekend []int) (time.Time, error) {
	if rule == "" || rule == ObservedNone {
		return date, nil
	}
	if !ValidObservedRule(rule) {
		return time.Time{}, oblBad("unknown observed_rule %q", rule)
	}
	isWeekend := func(d time.Time) bool { return weekendDay(weekend, d) }
	if !isWeekend(date) {
		return date, nil
	}
	step := func(dir int) (time.Time, int, bool) {
		d := date
		for i := 1; i <= 7; i++ {
			d = d.AddDate(0, 0, dir)
			if !isWeekend(d) {
				return d, i, true
			}
		}
		return time.Time{}, 0, false
	}
	next, nextDist, okNext := step(1)
	prev, prevDist, okPrev := step(-1)
	if !okNext || !okPrev {
		return time.Time{}, oblBad("a calendar whose every day is a weekend has no weekday to observe a holiday on")
	}
	switch rule {
	case ObservedNextWeekday:
		return next, nil
	case ObservedPreviousWeekday:
		return prev, nil
	}
	if prevDist < nextDist {
		return prev, nil
	}
	return next, nil
}

func weekendDay(weekend []int, d time.Time) bool {
	for _, w := range weekend {
		if int(d.Weekday()) == w {
			return true
		}
	}
	return false
}

// NonBusinessDay says whether d is not a business day of this calendar version
// and why. A holiday is non-business on the date it is observed.
func (c CalendarVersion) NonBusinessDay(d time.Time) (reason, name string, non bool) {
	if c.isWeekend(d) {
		return ReasonWeekend, "", true
	}
	if n, ok := c.holidayOn(d); ok {
		return ReasonHoliday, n, true
	}
	return "", "", false
}

// IsBusinessDay reports whether d is a business day of this calendar version.
func (c CalendarVersion) IsBusinessDay(d time.Time) bool {
	_, _, non := c.NonBusinessDay(d)
	return !non
}

func (c CalendarVersion) skipped(d time.Time) SkippedDay {
	reason, name, _ := c.NonBusinessDay(d)
	return SkippedDay{Date: d.Format(dateLayout), Reason: reason, Name: name}
}

// roll moves d in dir (+1 or -1) to the first business day, returning the days
// passed over.
func (c CalendarVersion) roll(d time.Time, dir int) (time.Time, []SkippedDay, error) {
	var skipped []SkippedDay
	for i := 0; i <= maxConsecutiveNonBusiness; i++ {
		if c.IsBusinessDay(d) {
			return d, skipped, nil
		}
		skipped = append(skipped, c.skipped(d))
		d = d.AddDate(0, 0, dir)
	}
	return time.Time{}, nil, oblBad("no business day within %d days of %s: the calendar has no business days", maxConsecutiveNonBusiness, d.Format(dateLayout))
}

// Adjust applies a business-day convention to d: a business day is returned
// unchanged; any other is moved as the convention says, and the days it passed
// over are returned.
//
//	NONE               unchanged, even if it is not a business day
//	FOLLOWING          the next business day
//	PRECEDING          the previous business day
//	MODIFIED_FOLLOWING the next, unless that is in another month, then the previous
//	MODIFIED_PRECEDING the previous, unless that is in another month, then the next
func (c CalendarVersion) Adjust(d time.Time, convention string) (time.Time, []SkippedDay, error) {
	if !ValidConvention(convention) {
		return time.Time{}, nil, oblBad("unknown business-day convention %q", convention)
	}
	if convention == ConventionNone || c.IsBusinessDay(d) {
		return d, nil, nil
	}
	switch convention {
	case ConventionFollowing:
		return c.roll(d, 1)
	case ConventionPreceding:
		return c.roll(d, -1)
	case ConventionModifiedFollowing:
		out, skipped, err := c.roll(d, 1)
		if err == nil && out.Month() == d.Month() {
			return out, skipped, nil
		}
		return c.roll(d, -1)
	default: // MODIFIED_PRECEDING
		out, skipped, err := c.roll(d, -1)
		if err == nil && out.Month() == d.Month() {
			return out, skipped, nil
		}
		return c.roll(d, 1)
	}
}

// AddBusinessDays moves n business days from d (negative n moves back). The
// start date is never counted, whether or not it is a business day: adding one
// business day to a Friday is Monday, and to a Saturday is Monday too. Zero
// returns d unchanged. The days passed over are returned.
func (c CalendarVersion) AddBusinessDays(d time.Time, n int) (time.Time, []SkippedDay, error) {
	if n > MaxBusinessDayShift || n < -MaxBusinessDayShift {
		return time.Time{}, nil, oblBad("days must be between %d and %d", -MaxBusinessDayShift, MaxBusinessDayShift)
	}
	if n == 0 {
		return d, nil, nil
	}
	dir, left := 1, n
	if n < 0 {
		dir, left = -1, -n
	}
	var skipped []SkippedDay
	run := 0
	for left > 0 {
		d = d.AddDate(0, 0, dir)
		if c.IsBusinessDay(d) {
			left--
			run = 0
			continue
		}
		run++
		if run > maxConsecutiveNonBusiness {
			return time.Time{}, nil, oblBad("no business day within %d days of %s: the calendar has no business days", maxConsecutiveNonBusiness, d.Format(dateLayout))
		}
		skipped = append(skipped, c.skipped(d))
	}
	return d, skipped, nil
}

// CalendarContentDigest is the digest of what a calendar version says: its
// identity, effective date, time zone, weekend, cut-off and holidays with their
// observed dates. Two versions with the same digest give the same answers.
func CalendarContentDigest(c CalendarVersion) string {
	wk := append([]int{}, c.WeekendDays...)
	sort.Ints(wk)
	hs := append([]Holiday(nil), c.Holidays...)
	sort.Slice(hs, func(i, j int) bool { return hs[i].Date < hs[j].Date })
	rows := make([]map[string]string, 0, len(hs))
	for _, h := range hs {
		rows = append(rows, map[string]string{"date": h.Date, "name": h.Name, "observed_rule": h.ObservedRule, "observed_date": h.ObservedDate})
	}
	cutoff := ""
	if c.CutoffTime != nil {
		cutoff = *c.CutoffTime
	}
	raw, _ := json.Marshal(map[string]any{
		"calendar_code": c.CalendarCode, "version": c.Version, "effective_from": c.EffectiveFrom.Format(dateLayout),
		"timezone": c.Timezone, "weekend_days": wk, "cutoff_time": cutoff, "holidays": rows,
	})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ── ImportHolidayCalendar ───────────────────────────────────────────────────

// Bounds on an import.
const (
	MaxImportHolidays = 400
	maxImportText     = 512
)

// ImportCalendarParams is the ImportHolidayCalendar command: a holiday list from
// an authority, as a new draft version of a calendar. "Source holiday authority"
// and the source's own version are the evidence; the content hash is computed.
type ImportCalendarParams struct {
	CalendarCode    string
	SourceAuthority string
	SourceReference string
	// SourceVersion is the authority's own identifier for this edition of the
	// list, for example "2026-2". One source version is imported once.
	SourceVersion string

	EffectiveFrom string
	Timezone      string
	WeekendDays   []int
	CutoffTime    *string
	Holidays      []Holiday

	// SourceIDs cite registered regulatory sources, which publishing requires.
	SourceIDs []string
	Actor     string
}

// Validate checks the import as a whole.
func (p ImportCalendarParams) Validate() error {
	if !ValidCalendarCode(p.CalendarCode) {
		return oblBad("calendar_code is not a valid calendar code")
	}
	for field, v := range map[string]string{"source_authority": p.SourceAuthority, "source_reference": p.SourceReference, "source_version": p.SourceVersion} {
		if strings.TrimSpace(v) == "" {
			return oblBad("%s is required: an import names its authority, where it came from and which edition", field)
		}
		if len(v) > maxImportText {
			return oblBad("%s is longer than %d characters", field, maxImportText)
		}
	}
	if len(p.SourceVersion) > 64 {
		return oblBad("source_version is longer than 64 characters")
	}
	if strings.TrimSpace(p.Actor) == "" {
		return oblBad("an actor is required")
	}
	if len(p.Holidays) == 0 {
		return oblBad("an import needs at least one holiday")
	}
	if len(p.Holidays) > MaxImportHolidays {
		return oblBad("an import carries at most %d holidays", MaxImportHolidays)
	}
	from, err := time.Parse(dateLayout, p.EffectiveFrom)
	if err != nil {
		return oblBad("effective_from must be YYYY-MM-DD")
	}
	return ValidateCalendarVersion(CalendarVersion{
		EffectiveFrom: from, Timezone: p.Timezone, WeekendDays: p.WeekendDays, CutoffTime: p.CutoffTime, Holidays: p.Holidays,
	})
}

// WithObservedDates returns the holidays with each one's observed date worked
// out from its rule and the calendar's weekend, so the stored calendar never
// depends on the caller's arithmetic.
func WithObservedDates(holidays []Holiday, weekend []int) ([]Holiday, error) {
	out := make([]Holiday, len(holidays))
	for i, h := range holidays {
		h.Name = strings.TrimSpace(h.Name)
		h.ObservedDate = ""
		if h.ObservedRule == "" {
			h.ObservedRule = ObservedNone
		}
		date, err := time.Parse(dateLayout, h.Date)
		if err != nil {
			return nil, oblBad("holiday date %q must be YYYY-MM-DD", h.Date)
		}
		if h.ObservedRule != ObservedNone {
			obs, err := ComputeObservedDate(date, h.ObservedRule, weekend)
			if err != nil {
				return nil, err
			}
			h.ObservedDate = obs.Format(dateLayout)
		}
		out[i] = h
	}
	return out, nil
}

// CalendarImportHash is the SHA-256 of what an import says: the authority, its
// edition and the calendar content. The reference (where it was fetched from) is
// not part of it, so the same edition from another mirror is the same import.
func CalendarImportHash(p ImportCalendarParams) string {
	wk := append([]int{}, p.WeekendDays...)
	sort.Ints(wk)
	hs := make([]map[string]string, 0, len(p.Holidays))
	sorted := append([]Holiday(nil), p.Holidays...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Date < sorted[j].Date })
	for _, h := range sorted {
		rule := h.ObservedRule
		if rule == "" {
			rule = ObservedNone
		}
		hs = append(hs, map[string]string{"date": h.Date, "name": strings.TrimSpace(h.Name), "observed_rule": rule})
	}
	cutoff := ""
	if p.CutoffTime != nil {
		cutoff = *p.CutoffTime
	}
	raw, _ := json.Marshal(map[string]any{
		"source_authority": strings.TrimSpace(p.SourceAuthority), "source_version": strings.TrimSpace(p.SourceVersion),
		"effective_from": p.EffectiveFrom, "timezone": p.Timezone, "weekend_days": wk, "cutoff_time": cutoff, "holidays": hs,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ── answers ─────────────────────────────────────────────────────────────────

// CalendarProvenance says which calendar version answered, so the answer can be
// reproduced: "calculation returns applied version/provenance".
type CalendarProvenance struct {
	CalendarCode      string  `json:"calendar_code"`
	CalendarVersion   int     `json:"calendar_version"`
	CalendarVersionID string  `json:"calendar_version_id"`
	ContentDigest     string  `json:"content_digest"`
	Timezone          string  `json:"timezone"`
	EffectiveFrom     string  `json:"effective_from"`
	SourceAuthority   *string `json:"source_authority,omitempty"`
	SourceVersion     *string `json:"source_version,omitempty"`
	ImportHash        *string `json:"import_hash,omitempty"`
	// Pinned is true when the caller named the version rather than asking for
	// the one in force.
	Pinned bool `json:"pinned"`
}

// BusinessDayCheck is the answer to IsBusinessDay.
type BusinessDayCheck struct {
	Date          string             `json:"date"`
	IsBusinessDay bool               `json:"is_business_day"`
	Reason        string             `json:"reason,omitempty"`
	HolidayName   string             `json:"holiday_name,omitempty"`
	Calendar      CalendarProvenance `json:"calendar"`
}

// AddBusinessDaysResult is the answer to AddBusinessDays.
type AddBusinessDaysResult struct {
	StartDate string             `json:"start_date"`
	Days      int                `json:"days"`
	Result    string             `json:"result_date"`
	Skipped   []SkippedDay       `json:"skipped"`
	Calendar  CalendarProvenance `json:"calendar"`
}

// HolidayChange is one holiday occurrence that differs from the previous
// published version of the calendar.
type HolidayChange struct {
	Date         string `json:"date"`
	Change       string `json:"change"` // ADDED | REMOVED | CHANGED
	Name         string `json:"name,omitempty"`
	PreviousName string `json:"previous_name,omitempty"`
	ObservedDate string `json:"observed_date,omitempty"`
	// PreviousObservedDate is where it used to be observed, if that differs.
	PreviousObservedDate string `json:"previous_observed_date,omitempty"`
}

// DiffHolidays lists what the holidays of a calendar version change relative to
// the previous version's, by holiday date, in date order. A holiday whose name or
// observed date changed is CHANGED.
func DiffHolidays(previous, current []Holiday) []HolidayChange {
	prev := map[string]Holiday{}
	for _, h := range previous {
		prev[h.Date] = h
	}
	cur := map[string]Holiday{}
	for _, h := range current {
		cur[h.Date] = h
	}
	dates := map[string]bool{}
	for d := range prev {
		dates[d] = true
	}
	for d := range cur {
		dates[d] = true
	}
	ordered := make([]string, 0, len(dates))
	for d := range dates {
		ordered = append(ordered, d)
	}
	sort.Strings(ordered)
	var out []HolidayChange
	for _, d := range ordered {
		p, hadPrev := prev[d]
		c, hasCur := cur[d]
		switch {
		case !hadPrev:
			out = append(out, HolidayChange{Date: d, Change: "ADDED", Name: c.Name, ObservedDate: c.observedOn()})
		case !hasCur:
			out = append(out, HolidayChange{Date: d, Change: "REMOVED", PreviousName: p.Name, PreviousObservedDate: p.observedOn()})
		case p.Name != c.Name || p.observedOn() != c.observedOn():
			ch := HolidayChange{Date: d, Change: "CHANGED", Name: c.Name, PreviousName: p.Name, ObservedDate: c.observedOn()}
			if p.observedOn() != c.observedOn() {
				ch.PreviousObservedDate = p.observedOn()
			}
			out = append(out, ch)
		}
	}
	return out
}

var (
	// ErrCalendarImportConflict: this source version of the calendar was already
	// imported, with different content. The same content is a replay.
	ErrCalendarImportConflict = errorString("this source version of the calendar was already imported with different content")
	// ErrNoCalendarVersion: no published version of the calendar governs the
	// date at the knowledge time asked about.
	ErrNoCalendarVersion = errorString("no published version of the calendar governs that date")
)
