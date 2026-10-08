package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // authority time zones must resolve on minimal hosts and containers
)

// ZS-JUR-001 Wave 4 (calendar half): the regulatory calendar and obligation
// due-date engine (s15, s16).
//
// Due dates are DERIVED, never hand-entered: from an effective-dated
// obligation rule (period basis, anchor, offset, business-day adjustment,
// extension policy, cutoff) and a versioned authority calendar (weekend days,
// holidays, time zone). The calculation is a pure function of the rule, the
// calendar version and the facts, so it is reproducible, and it records every
// step so the result is explainable.
//
// Order of operations (an assumption the document leaves open, stated here
// once): anchor date -> add offset months (clamping to month end) -> add offset
// days -> add a granted extension -> adjust to a business day -> apply the
// authority-local cutoff.

// Period bases, anchors and business-day adjustments.
const (
	BasisMonthly      = "MONTHLY"
	BasisQuarterly    = "QUARTERLY"
	BasisAnnual       = "ANNUAL"
	BasisEventDriven  = "EVENT_DRIVEN"
	BasisPayrollCycle = "PAYROLL_CYCLE"

	AnchorPeriodEnd        = "PERIOD_END"
	AnchorEventDate        = "EVENT_DATE"
	AnchorRegistrationDate = "REGISTRATION_DATE"
	AnchorAnniversary      = "ANNIVERSARY"

	AdjustNone     = "NONE"
	AdjustNext     = "NEXT"
	AdjustPrevious = "PREVIOUS"
)

// Due-date outcomes.
const (
	DueCalculated          = "DUE_DATE_CALCULATED"
	DueNoRule              = "NO_OBLIGATION_RULE"
	DueUnsupported         = OutcomeUnsupported
	DueInvalidFacts        = "INVALID_FACTS"
	DueExtensionNotAllowed = "EXTENSION_NOT_PERMITTED"
	DueNoCalendar          = "NO_CALENDAR"
	DueAmbiguous           = OutcomeAmbiguous
)

const dateLayout = "2006-01-02"

// Holiday is one non-business day of a calendar version.
type Holiday struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

// CalendarVersion is one immutable, effective-dated version of an authority
// calendar. WeekendDays uses 0 = Sunday ... 6 = Saturday (time.Weekday).
type CalendarVersion struct {
	CalendarVersionID string
	CalendarCode      string
	Version           int
	EffectiveFrom     time.Time // start of the first date this version governs
	Timezone          string
	WeekendDays       []int
	CutoffTime        *string // "HH:MM" authority-local, optional
	Holidays          []Holiday
	ContentDigest     string
	SourceIDs         []string
}

// ObligationRule is one immutable, effective-dated due-date rule.
type ObligationRule struct {
	ObligationRuleID string
	JurisdictionID   string
	ObligationCode   string
	RuleVersion      int
	Name             string
	PeriodBasis      string
	Anchor           string
	OffsetMonths     int
	OffsetDays       int
	// OffsetToMonthEnd moves the date to the last day of the month reached after
	// OffsetMonths (the "end of the next month plus N days" pattern, e.g. UK VAT).
	// Without it the day-of-month is kept and clamped (30 Sep + 1 month = 30 Oct).
	OffsetToMonthEnd          bool
	BusinessDayAdjustment     string
	CalendarCode              string
	EffectiveFrom             time.Time
	EffectiveTo               *time.Time
	ExtensionAllowed          bool
	MaxExtensionDays          int
	ExtensionRequiresEvidence bool
	CutoffApplies             bool
	EscalationOwner           *string
	EscalationSLAHours        *int
	RegimeID                  *string
	InterpretationID          *string
	ContentDigest             string
	SourceIDs                 []string
}

// DueFacts are the authoritative facts a due date depends on. Only the fact
// the rule's anchor needs must be present.
type DueFacts struct {
	PeriodEnd         string `json:"period_end,omitempty"`
	EventDate         string `json:"event_date,omitempty"`
	RegistrationDate  string `json:"registration_date,omitempty"`
	AnniversaryDate   string `json:"anniversary_date,omitempty"`
	AnchorYear        int    `json:"anchor_year,omitempty"`
	ExtensionDays     int    `json:"extension_days,omitempty"`
	ExtensionEvidence string `json:"extension_evidence_ref,omitempty"`
}

// DueResult is the explainable outcome of a due-date calculation.
type DueResult struct {
	Outcome           string     `json:"outcome"`
	Message           string     `json:"message,omitempty"`
	ObligationRuleID  string     `json:"obligation_rule_id,omitempty"`
	ObligationCode    string     `json:"obligation_code,omitempty"`
	RuleVersion       int        `json:"rule_version,omitempty"`
	ContentDigest     string     `json:"content_digest,omitempty"`
	AnchorDate        string     `json:"anchor_date,omitempty"`
	UnadjustedDate    string     `json:"unadjusted_date,omitempty"`
	ExtendedDate      string     `json:"extended_date,omitempty"`
	DueDate           string     `json:"due_date,omitempty"`
	DueAt             *time.Time `json:"due_at,omitempty"`
	Timezone          string     `json:"timezone,omitempty"`
	CalendarCode      string     `json:"calendar_code,omitempty"`
	CalendarVersion   int        `json:"calendar_version,omitempty"`
	CalendarVersionID string     `json:"calendar_version_id,omitempty"`
	Adjusted          bool       `json:"adjusted"`
	ExtensionGranted  int        `json:"extension_days_granted,omitempty"`
	EscalationOwner   *string    `json:"escalation_owner,omitempty"`
	EscalationSLAHrs  *int       `json:"escalation_sla_hours,omitempty"`
	Explanation       []string   `json:"explanation"`
}

// OverdueAt reports whether the due instant has passed at now.
func (r DueResult) OverdueAt(now time.Time) bool { return r.DueAt != nil && now.After(*r.DueAt) }

var (
	obligationCodeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	calendarCodeRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
	hhmmRe           = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// ValidCalendarCode and ValidObligationCode expose the identifier shapes.
func ValidCalendarCode(s string) bool   { return calendarCodeRe.MatchString(s) }
func ValidObligationCode(s string) bool { return obligationCodeRe.MatchString(s) }

// ErrObligationInvalid is returned for a malformed calendar or obligation rule.
var ErrObligationInvalid = errorString("calendar or obligation definition is invalid")

func oblBad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrObligationInvalid, fmt.Sprintf(format, a...))
}

// ValidateCalendarVersion checks a calendar version's shape.
func ValidateCalendarVersion(cv CalendarVersion) error {
	if _, err := time.LoadLocation(cv.Timezone); err != nil || strings.TrimSpace(cv.Timezone) == "" {
		return oblBad("timezone %q is not a known IANA time zone", cv.Timezone)
	}
	if len(cv.WeekendDays) > 6 {
		return oblBad("at least one weekday must remain a business day")
	}
	seen := map[int]bool{}
	for _, d := range cv.WeekendDays {
		if d < 0 || d > 6 {
			return oblBad("weekend_days entries are 0 (Sunday) to 6 (Saturday), got %d", d)
		}
		if seen[d] {
			return oblBad("weekend_days repeats %d", d)
		}
		seen[d] = true
	}
	if cv.CutoffTime != nil && !hhmmRe.MatchString(*cv.CutoffTime) {
		return oblBad("cutoff_time must be HH:MM, got %q", *cv.CutoffTime)
	}
	dates := map[string]bool{}
	for _, h := range cv.Holidays {
		if _, err := time.Parse(dateLayout, h.Date); err != nil {
			return oblBad("holiday date %q must be YYYY-MM-DD", h.Date)
		}
		if strings.TrimSpace(h.Name) == "" {
			return oblBad("holiday %s needs a name", h.Date)
		}
		if dates[h.Date] {
			return oblBad("holiday %s is listed twice", h.Date)
		}
		dates[h.Date] = true
	}
	return nil
}

// ValidateObligationRule checks an obligation rule's shape.
func ValidateObligationRule(r ObligationRule) error {
	if !obligationCodeRe.MatchString(r.ObligationCode) {
		return oblBad("obligation_code %q must be UPPER_SNAKE", r.ObligationCode)
	}
	if strings.TrimSpace(r.Name) == "" {
		return oblBad("name is required")
	}
	switch r.PeriodBasis {
	case BasisMonthly, BasisQuarterly, BasisAnnual, BasisEventDriven, BasisPayrollCycle:
	default:
		return oblBad("period_basis must be MONTHLY, QUARTERLY, ANNUAL, EVENT_DRIVEN or PAYROLL_CYCLE")
	}
	switch r.Anchor {
	case AnchorPeriodEnd, AnchorEventDate, AnchorRegistrationDate, AnchorAnniversary:
	default:
		return oblBad("anchor must be PERIOD_END, EVENT_DATE, REGISTRATION_DATE or ANNIVERSARY")
	}
	if r.OffsetMonths < 0 || r.OffsetMonths > 60 || r.OffsetDays < 0 || r.OffsetDays > 400 {
		return oblBad("offsets must be 0-60 months and 0-400 days")
	}
	switch r.BusinessDayAdjustment {
	case AdjustNone, AdjustNext, AdjustPrevious:
	default:
		return oblBad("business_day_adjustment must be NONE, NEXT or PREVIOUS")
	}
	if r.OffsetToMonthEnd && r.OffsetMonths == 0 {
		return oblBad("offset_to_month_end needs offset_months of at least 1")
	}
	if r.BusinessDayAdjustment != AdjustNone && r.CalendarCode == "" {
		return oblBad("a business-day adjustment needs a calendar_code")
	}
	if r.CalendarCode != "" && !calendarCodeRe.MatchString(r.CalendarCode) {
		return oblBad("calendar_code %q is not a valid calendar code", r.CalendarCode)
	}
	if r.CutoffApplies && r.CalendarCode == "" {
		return oblBad("cutoff_applies needs a calendar_code (the cutoff and time zone live in the calendar)")
	}
	if r.MaxExtensionDays < 0 || r.MaxExtensionDays > 366 {
		return oblBad("max_extension_days must be 0-366")
	}
	if !r.ExtensionAllowed && (r.MaxExtensionDays != 0 || r.ExtensionRequiresEvidence) {
		return oblBad("extension settings are meaningless when extension_allowed is false (s15: extensions only where legally supported)")
	}
	if r.ExtensionAllowed && r.MaxExtensionDays == 0 {
		return oblBad("extension_allowed needs max_extension_days of at least 1")
	}
	if r.EscalationSLAHours != nil && *r.EscalationSLAHours < 1 {
		return oblBad("escalation_sla_hours must be at least 1")
	}
	if r.EffectiveTo != nil && !r.EffectiveTo.After(r.EffectiveFrom) {
		return oblBad("effective_to must be after effective_from")
	}
	return nil
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse(dateLayout, s)
	return t, err == nil
}

func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// addMonths adds n calendar months and clamps to the end of a shorter month
// (31 January plus one month is the last day of February).
func addMonths(t time.Time, n int) time.Time {
	total := int(t.Month()) - 1 + n
	y := t.Year() + total/12
	m := time.Month(total%12 + 1)
	d := t.Day()
	if dim := daysInMonth(y, m); d > dim {
		d = dim
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func isMonthEnd(t time.Time) bool { return t.Day() == daysInMonth(t.Year(), t.Month()) }

// anchorFor returns the rule's anchor date from the facts, or a message why not.
func anchorFor(r ObligationRule, f DueFacts) (time.Time, string) {
	switch r.Anchor {
	case AnchorPeriodEnd:
		if f.PeriodEnd == "" {
			return time.Time{}, "this rule is anchored on PERIOD_END: period_end is required"
		}
		t, ok := parseDate(f.PeriodEnd)
		if !ok {
			return time.Time{}, "period_end must be YYYY-MM-DD"
		}
		switch r.PeriodBasis {
		case BasisMonthly:
			if !isMonthEnd(t) {
				return time.Time{}, "a MONTHLY obligation's period_end must be a month end"
			}
		case BasisQuarterly:
			if !isMonthEnd(t) || int(t.Month())%3 != 0 {
				return time.Time{}, "a QUARTERLY obligation's period_end must be a calendar quarter end (31 Mar, 30 Jun, 30 Sep, 31 Dec)"
			}
		}
		return t, ""
	case AnchorEventDate:
		if f.EventDate == "" {
			return time.Time{}, "this rule is anchored on EVENT_DATE: event_date is required"
		}
		t, ok := parseDate(f.EventDate)
		if !ok {
			return time.Time{}, "event_date must be YYYY-MM-DD"
		}
		return t, ""
	case AnchorRegistrationDate:
		if f.RegistrationDate == "" {
			return time.Time{}, "this rule is anchored on REGISTRATION_DATE: registration_date is required"
		}
		t, ok := parseDate(f.RegistrationDate)
		if !ok {
			return time.Time{}, "registration_date must be YYYY-MM-DD"
		}
		return t, ""
	default: // AnchorAnniversary
		if f.AnniversaryDate == "" || f.AnchorYear == 0 {
			return time.Time{}, "this rule is anchored on ANNIVERSARY: anniversary_date and anchor_year are required"
		}
		t, ok := parseDate(f.AnniversaryDate)
		if !ok {
			return time.Time{}, "anniversary_date must be YYYY-MM-DD"
		}
		if f.AnchorYear < 1900 || f.AnchorYear > 2200 {
			return time.Time{}, "anchor_year is out of range"
		}
		// The anniversary falls on the same month and day in the anchor year
		// (29 February clamps to 28 February in a common year).
		d := t.Day()
		if dim := daysInMonth(f.AnchorYear, t.Month()); d > dim {
			d = dim
		}
		return time.Date(f.AnchorYear, t.Month(), d, 0, 0, 0, 0, time.UTC), ""
	}
}

func (r ObligationRule) effectiveOn(d time.Time) bool {
	return !d.Before(r.EffectiveFrom) && (r.EffectiveTo == nil || d.Before(*r.EffectiveTo))
}

func (c CalendarVersion) isWeekend(d time.Time) bool {
	for _, w := range c.WeekendDays {
		if int(d.Weekday()) == w {
			return true
		}
	}
	return false
}

func (c CalendarVersion) holidayOn(d time.Time) (string, bool) {
	key := d.Format(dateLayout)
	for _, h := range c.Holidays {
		if h.Date == key {
			return h.Name, true
		}
	}
	return "", false
}

// pickCalendar chooses the calendar version in force for a date: among
// versions of the code effective on or before it, the highest version. An
// amendment is therefore a NEW version; earlier results keep the version they
// recorded (JUR-NEG-13).
func pickCalendar(cals []CalendarVersion, code string, on time.Time) *CalendarVersion {
	var best *CalendarVersion
	for i := range cals {
		c := &cals[i]
		if c.CalendarCode != code || c.EffectiveFrom.After(on) {
			continue
		}
		if best == nil || c.Version > best.Version {
			best = c
		}
	}
	return best
}

// CalculateDueDate computes the due date for one obligation rule. The caller
// has already chosen the rule; this function never guesses.
func CalculateDueDate(r ObligationRule, cals []CalendarVersion, f DueFacts) DueResult {
	res := DueResult{ObligationRuleID: r.ObligationRuleID, ObligationCode: r.ObligationCode, RuleVersion: r.RuleVersion,
		ContentDigest: r.ContentDigest, EscalationOwner: r.EscalationOwner, EscalationSLAHrs: r.EscalationSLAHours, Explanation: []string{}}
	step := func(format string, a ...any) { res.Explanation = append(res.Explanation, fmt.Sprintf(format, a...)) }
	fail := func(outcome, msg string) DueResult {
		res.Outcome, res.Message = outcome, msg
		step("%s: %s", outcome, msg)
		return res
	}

	anchor, why := anchorFor(r, f)
	if why != "" {
		return fail(DueInvalidFacts, why)
	}
	res.AnchorDate = anchor.Format(dateLayout)
	step("anchor %s = %s", r.Anchor, res.AnchorDate)

	shifted := addMonths(anchor, r.OffsetMonths)
	if r.OffsetToMonthEnd {
		shifted = time.Date(shifted.Year(), shifted.Month(), daysInMonth(shifted.Year(), shifted.Month()), 0, 0, 0, 0, time.UTC)
		step("plus %d month(s), to the end of that month = %s", r.OffsetMonths, shifted.Format(dateLayout))
	}
	unadjusted := shifted.AddDate(0, 0, r.OffsetDays)
	res.UnadjustedDate = unadjusted.Format(dateLayout)
	step("plus %d month(s) and %d day(s) = %s", r.OffsetMonths, r.OffsetDays, res.UnadjustedDate)

	extended := unadjusted
	if f.ExtensionDays != 0 {
		switch {
		case f.ExtensionDays < 0:
			return fail(DueInvalidFacts, "extension_days cannot be negative")
		case !r.ExtensionAllowed:
			return fail(DueExtensionNotAllowed, "this obligation does not permit an extension (s15: only where legally supported)")
		case f.ExtensionDays > r.MaxExtensionDays:
			return fail(DueExtensionNotAllowed, fmt.Sprintf("an extension of %d day(s) exceeds the permitted maximum of %d", f.ExtensionDays, r.MaxExtensionDays))
		case r.ExtensionRequiresEvidence && strings.TrimSpace(f.ExtensionEvidence) == "":
			return fail(DueExtensionNotAllowed, "an extension must be evidenced: extension_evidence_ref is required")
		}
		extended = unadjusted.AddDate(0, 0, f.ExtensionDays)
		res.ExtensionGranted = f.ExtensionDays
		step("extension of %d day(s) granted (evidence %q) = %s", f.ExtensionDays, f.ExtensionEvidence, extended.Format(dateLayout))
	}
	res.ExtendedDate = extended.Format(dateLayout)

	var cal *CalendarVersion
	if r.CalendarCode != "" {
		cal = pickCalendar(cals, r.CalendarCode, extended)
		if cal == nil {
			return fail(DueNoCalendar, fmt.Sprintf("no version of calendar %q is in force on %s", r.CalendarCode, res.ExtendedDate))
		}
		res.CalendarCode, res.CalendarVersion, res.CalendarVersionID, res.Timezone = cal.CalendarCode, cal.Version, cal.CalendarVersionID, cal.Timezone
		step("calendar %s version %d (time zone %s)", cal.CalendarCode, cal.Version, cal.Timezone)
	}

	due := extended
	if r.BusinessDayAdjustment != AdjustNone && cal != nil {
		dir := 1
		if r.BusinessDayAdjustment == AdjustPrevious {
			dir = -1
		}
		for i := 0; i < 366; i++ {
			switch {
			case cal.isWeekend(due):
				step("%s is a weekend day", due.Format(dateLayout))
			default:
				if name, ok := cal.holidayOn(due); ok {
					step("%s is a holiday (%s)", due.Format(dateLayout), name)
				} else {
					goto settled
				}
			}
			due = due.AddDate(0, 0, dir)
			res.Adjusted = true
		}
		return fail(DueNoCalendar, "no business day found within a year: the calendar has no business days")
	}
settled:
	res.DueDate = due.Format(dateLayout)
	if res.Adjusted {
		step("moved %s to the %s business day = %s", extended.Format(dateLayout), strings.ToLower(r.BusinessDayAdjustment), res.DueDate)
	} else if r.BusinessDayAdjustment != AdjustNone {
		step("%s is already a business day", res.DueDate)
	}

	// The instant: the end of the authority-local day, or the calendar cutoff
	// when the rule applies it.
	tz := "UTC"
	h, mi, s, ns := 23, 59, 59, 999999999
	if cal != nil {
		tz = cal.Timezone
		if r.CutoffApplies && cal.CutoffTime != nil {
			fmt.Sscanf(*cal.CutoffTime, "%d:%d", &h, &mi)
			s, ns = 0, 0
			step("authority cutoff %s", *cal.CutoffTime)
		}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fail(DueNoCalendar, "time zone "+tz+" cannot be loaded")
	}
	at := time.Date(due.Year(), due.Month(), due.Day(), h, mi, s, ns, loc).UTC()
	res.DueAt = &at
	if res.Timezone == "" {
		res.Timezone = "UTC"
	}
	step("due at %s (%s local end of day or cutoff)", at.Format(time.RFC3339Nano), res.Timezone)
	res.Outcome = DueCalculated
	return res
}

// SelectObligationRule picks the obligation rule for a code in a jurisdiction
// chain (the jurisdiction and its parents present in rules' own jurisdiction
// ids), effective on the rule's own anchor date. More than one is AMBIGUOUS:
// obligation rules carry no precedence, so an overlap is never resolved by guess.
func SelectObligationRule(rules []ObligationRule, chain map[string]bool, code string, f DueFacts) (*ObligationRule, DueResult) {
	var cands []ObligationRule
	var factFailure string
	for _, r := range rules {
		if r.ObligationCode != code || !chain[r.JurisdictionID] {
			continue
		}
		anchor, why := anchorFor(r, f)
		if why != "" {
			if factFailure == "" {
				factFailure = why
			}
			continue
		}
		if r.effectiveOn(anchor) {
			cands = append(cands, r)
		}
	}
	switch len(cands) {
	case 0:
		if factFailure != "" {
			return nil, DueResult{Outcome: DueInvalidFacts, Message: factFailure, Explanation: []string{DueInvalidFacts + ": " + factFailure}}
		}
		return nil, DueResult{Outcome: DueNoRule, Message: "no obligation rule " + code + " is effective on the anchor date",
			Explanation: []string{DueNoRule + ": no obligation rule " + code + " is effective on the anchor date"}}
	case 1:
		return &cands[0], DueResult{}
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ObligationRuleID)
	}
	sort.Strings(ids)
	msg := "overlapping obligation rules: " + strings.Join(ids, ", ")
	return nil, DueResult{Outcome: DueAmbiguous, Message: msg, Explanation: []string{DueAmbiguous + ": " + msg}}
}

// JurisdictionChain returns the ids of the entry and its parents within scope.
func JurisdictionChain(scope []ArtifactScopeEntry, code string) (map[string]bool, bool) {
	byID := map[string]ArtifactScopeEntry{}
	var start *ArtifactScopeEntry
	for i := range scope {
		byID[scope[i].ID] = scope[i]
		if scope[i].Code == code {
			start = &scope[i]
		}
	}
	if start == nil {
		return nil, false
	}
	chain := map[string]bool{}
	for cur, hops := start, 0; cur != nil && hops < 64; hops++ {
		chain[cur.ID] = true
		var next *ArtifactScopeEntry
		if cur.ParentID != nil {
			if p, ok := byID[*cur.ParentID]; ok && !chain[p.ID] {
				next = &p
			}
		}
		cur = next
	}
	return chain, true
}

// SelectionDate returns the date used to pick the pack version in force for a
// calculation: the first anchor fact present (period end, event date,
// registration date, then the anniversary in the anchor year). The obligation
// rule itself is still chosen on its own anchor date inside the artifact.
func SelectionDate(f DueFacts) (time.Time, bool) {
	for _, s := range []string{f.PeriodEnd, f.EventDate, f.RegistrationDate} {
		if s != "" {
			t, ok := parseDate(s)
			return t, ok
		}
	}
	if f.AnniversaryDate != "" && f.AnchorYear != 0 {
		if t, ok := parseDate(f.AnniversaryDate); ok && f.AnchorYear >= 1900 && f.AnchorYear <= 2200 {
			d := t.Day()
			if dim := daysInMonth(f.AnchorYear, t.Month()); d > dim {
				d = dim
			}
			return time.Date(f.AnchorYear, t.Month(), d, 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}
