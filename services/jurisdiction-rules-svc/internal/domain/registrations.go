package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ZS-JUR-001 s16: registration, licence and scheme configuration and threshold
// monitors, as four more closed parameter families on the governed rule module
// (see jurisdiction-pack-decision-rule-parameters.md). They inherit provenance,
// independent publication, signing, certification, rollout and rollback.
//
//	REGISTRATION_TYPE {"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"HMRC","identifier":{"pattern":"^GB[0-9]{9}$",
//	                   "normalize":"UPPER_STRIP_SEPARATORS","examples_valid":["GB123456789"],"examples_invalid":["GB12345"]}}}
//	LICENCE_TYPE      {"family":"LICENCE_TYPE","licence":{"authority":"...","term":{"years":1},"renewal_notice_days":60,"grace_days":0}}
//	SCHEME            {"family":"SCHEME","scheme":{"authority":"...","eligibility":{"metric":"ANNUAL_TURNOVER","operator":"LTE","threshold":"150000"},
//	                   "lock_in":{"years":1},"cancellation_notice_days":30}}
//	THRESHOLD_MONITOR {"family":"THRESHOLD_MONITOR","monitor":{"metric":"TAXABLE_TURNOVER","period":{"kind":"ROLLING","months":12},"operator":"GT",
//	                   "threshold":"90000","breach":{"effect":"REGISTRATION_REQUIRED","effective":{"kind":"END_OF_MONTH_FOLLOWING"}}}}
//
// Boundary: the rule content (what a registration number looks like, how long a
// licence runs, when a threshold is breached) is governed here. The registrations
// themselves (an entity's actual VAT number, licence or election) are tenant facts
// owned by tenant-entity-registry-svc and the Tax domain: this service never stores
// one, never registers anyone and never contacts an authority. Callers send the
// facts; the answer is a verdict with its explanation, recorded as evidence.

const (
	FamilyRegistrationType = "REGISTRATION_TYPE"
	FamilyLicenceType      = "LICENCE_TYPE"
	FamilyScheme           = "SCHEME"
	FamilyThresholdMonitor = "THRESHOLD_MONITOR"

	RegistrationTypeDomain = "REGISTRATION_TYPE"
	LicenceTypeDomain      = "LICENCE_TYPE"
	SchemeDomain           = "SCHEME_ELECTION"
	ThresholdDomain        = "THRESHOLD_MONITOR"

	OutcomeIdentifierValid     = "IDENTIFIER_VALID"
	OutcomeIdentifierInvalid   = "IDENTIFIER_INVALID"
	OutcomeLicenceEvaluated    = "LICENCE_EVALUATED"
	OutcomeSchemeEligible      = "SCHEME_ELIGIBLE"
	OutcomeSchemeNotEligible   = "SCHEME_NOT_ELIGIBLE"
	OutcomeThresholdEvaluated  = "THRESHOLD_EVALUATED"
	LicenceTermExceeded        = "LICENCE_TERM_EXCEEDED"
	maxObservations            = 5000
	maxIdentifierLength        = 64
	maxIdentifierPatternLength = 200
	maxExamples                = 10
)

// Identifier normalisation modes.
const (
	NormalizeNone  = "NONE"
	NormalizeUpper = "UPPER"
	NormalizeStrip = "UPPER_STRIP_SEPARATORS"
)

// Licence statuses.
const (
	LicenceNotYetValid    = "NOT_YET_VALID"
	LicenceActive         = "ACTIVE"
	LicenceRenewalWindow  = "RENEWAL_WINDOW"
	LicenceInGrace        = "IN_GRACE"
	LicenceExpired        = "EXPIRED"
	breachRegistration    = "REGISTRATION_REQUIRED"
	breachReview          = "REVIEW_REQUIRED"
	breachNotify          = "NOTIFY_ONLY"
	periodStatic          = "STATIC"
	periodRolling         = "ROLLING"
	effectImmediate       = "IMMEDIATE"
	effectNextPeriodStart = "NEXT_PERIOD_START"
	effectDaysAfter       = "DAYS_AFTER"
	effectEndOfNextMonth  = "END_OF_MONTH_FOLLOWING"
)

var (
	normalizeModes = []string{NormalizeNone, NormalizeUpper, NormalizeStrip}
	operators      = []string{"LT", "LTE", "GT", "GTE"}
	breachEffects  = []string{breachRegistration, breachReview, breachNotify}
	effectKinds    = []string{effectImmediate, effectNextPeriodStart, effectDaysAfter, effectEndOfNextMonth}
	currencyRe     = regexp.MustCompile(`^[A-Z]{3}$`)
	dayLayout      = "2006-01-02"
)

// IdentifierFormat is how an authority's identifier is written. Matching uses Go's
// RE2 engine, which runs in linear time, so a pack author cannot ship a pattern that
// hangs the service.
type IdentifierFormat struct {
	Pattern         string   `json:"pattern"`
	Normalize       string   `json:"normalize"`
	ExamplesValid   []string `json:"examples_valid"`
	ExamplesInvalid []string `json:"examples_invalid"`
}

// RegistrationType is an s16 Registration Type.
type RegistrationType struct {
	Kind             string           `json:"kind"`
	Authority        string           `json:"authority"`
	Identifier       IdentifierFormat `json:"identifier"`
	EvidenceRequired []string         `json:"evidence_required,omitempty"`
	// ThresholdMonitor optionally names the THRESHOLD_MONITOR rule code that triggers this registration.
	ThresholdMonitor string `json:"threshold_monitor,omitempty"`
	// Obligations are the obligation codes that apply to an entity holding this registration (s16 registration-driven applicability).
	// Each should be defined as an obligation rule in the pack; the compiler warns (JUR-W031) when it is not.
	Obligations []string `json:"obligations,omitempty"`
}

// LicenceType is an s16 License/Permit definition.
type LicenceType struct {
	Authority string `json:"authority"`
	// Term is the maximum validity from issue. nil means the authority states the expiry on each licence.
	Term              *Duration `json:"term,omitempty"`
	RenewalNoticeDays int       `json:"renewal_notice_days"`
	GraceDays         int       `json:"grace_days"`
	EvidenceRequired  []string  `json:"evidence_required,omitempty"`
}

// Eligibility is a single closed comparison of a metric to a threshold.
type Eligibility struct {
	Metric    string `json:"metric"`
	Operator  string `json:"operator"`
	Threshold string `json:"threshold"`
	Currency  string `json:"currency,omitempty"`
}

// SchemeType is an s16 Scheme/Election definition.
type SchemeType struct {
	Authority              string      `json:"authority"`
	Eligibility            Eligibility `json:"eligibility"`
	LockIn                 *Duration   `json:"lock_in,omitempty"`
	CancellationNoticeDays int         `json:"cancellation_notice_days"`
	EvidenceRequired       []string    `json:"evidence_required,omitempty"`
}

// MonitorPeriod is the window a threshold is measured over.
type MonitorPeriod struct {
	Kind string `json:"kind"`
	// Months is the length of a ROLLING window.
	Months int `json:"months,omitempty"`
	// StartMonth and StartDay are the yearly start of a STATIC period (for example 1 April).
	StartMonth int `json:"start_month,omitempty"`
	StartDay   int `json:"start_day,omitempty"`
}

// BreachEffective says when a breach takes legal effect.
type BreachEffective struct {
	Kind string `json:"kind"`
	Days int    `json:"days,omitempty"`
}

// Breach is what happens when the threshold is met.
type Breach struct {
	Effect    string          `json:"effect"`
	Effective BreachEffective `json:"effective"`
}

// ThresholdMonitor is an s16 Threshold Monitor definition.
type ThresholdMonitor struct {
	Metric    string        `json:"metric"`
	Period    MonitorPeriod `json:"period"`
	Operator  string        `json:"operator"`
	Threshold string        `json:"threshold"`
	Currency  string        `json:"currency,omitempty"`
	Breach    Breach        `json:"breach"`
}

func registrationFamily(f string) bool {
	return f == FamilyRegistrationType || f == FamilyLicenceType || f == FamilyScheme || f == FamilyThresholdMonitor
}

func (p RuleParameters) hasRegistrationFields() bool {
	return p.Registration != nil || p.Licence != nil || p.Scheme != nil || p.Monitor != nil
}

// validateRegistrationFamily validates one of the four s16 families: the family's own
// payload and nothing from any other family.
func (p RuleParameters) validateRegistrationFamily() error {
	if p.hasOtherFamilyFields("") || p.Profile != nil {
		return paramBad("%s takes only its own payload, not the fields of another family", p.Family)
	}
	count := 0
	for _, set := range []bool{p.Registration != nil, p.Licence != nil, p.Scheme != nil, p.Monitor != nil} {
		if set {
			count++
		}
	}
	if count != 1 {
		return paramBad("%s needs exactly its own payload object", p.Family)
	}
	switch p.Family {
	case FamilyRegistrationType:
		if p.Registration == nil {
			return paramBad("REGISTRATION_TYPE needs registration")
		}
		return p.Registration.validate()
	case FamilyLicenceType:
		if p.Licence == nil {
			return paramBad("LICENCE_TYPE needs licence")
		}
		return p.Licence.validate()
	case FamilyScheme:
		if p.Scheme == nil {
			return paramBad("SCHEME needs scheme")
		}
		return p.Scheme.validate()
	default:
		if p.Monitor == nil {
			return paramBad("THRESHOLD_MONITOR needs monitor")
		}
		return p.Monitor.validate()
	}
}

func validateAuthority(a string) error {
	if strings.TrimSpace(a) == "" || len(a) > 120 {
		return paramBad("authority is required (at most 120 characters)")
	}
	return nil
}

func validateEvidenceList(l []string) error {
	if len(l) > 10 {
		return paramBad("evidence_required has at most 10 entries")
	}
	for _, e := range l {
		if !recordClassRe.MatchString(e) {
			return paramBad("evidence_required entry %q must be UPPER_SNAKE", e)
		}
	}
	return nil
}

func validateDays(label string, d int) error {
	if d < 0 || d > 365 {
		return paramBad("%s must be 0 to 365", label)
	}
	return nil
}

// NormalizeIdentifier applies the declared normalisation before matching.
func NormalizeIdentifier(mode, s string) string {
	switch mode {
	case NormalizeUpper:
		return strings.ToUpper(strings.TrimSpace(s))
	case NormalizeStrip:
		s = strings.NewReplacer(" ", "", "-", "", ".", "", "/", "").Replace(strings.TrimSpace(s))
		return strings.ToUpper(s)
	}
	return s
}

func (r RegistrationType) validate() error {
	if !recordClassRe.MatchString(r.Kind) {
		return paramBad("registration.kind %q must be UPPER_SNAKE", r.Kind)
	}
	if err := validateAuthority(r.Authority); err != nil {
		return err
	}
	if err := validateEvidenceList(r.EvidenceRequired); err != nil {
		return err
	}
	if r.ThresholdMonitor != "" && !recordClassRe.MatchString(r.ThresholdMonitor) {
		return paramBad("registration.threshold_monitor %q must be an UPPER_SNAKE rule code", r.ThresholdMonitor)
	}
	if len(r.Obligations) > 20 {
		return paramBad("registration.obligations has at most 20 codes")
	}
	seenOb := map[string]bool{}
	for _, o := range r.Obligations {
		if !ValidObligationCode(o) || seenOb[o] {
			return paramBad("registration.obligations entry %q must be a distinct UPPER_SNAKE obligation code", o)
		}
		seenOb[o] = true
	}
	id := r.Identifier
	if !inList(normalizeModes, id.Normalize) {
		return paramBad("identifier.normalize must be one of %s", strings.Join(normalizeModes, ", "))
	}
	if len(id.Pattern) == 0 || len(id.Pattern) > maxIdentifierPatternLength || !strings.HasPrefix(id.Pattern, "^") || !strings.HasSuffix(id.Pattern, "$") {
		return paramBad("identifier.pattern must be anchored (start with ^, end with $) and at most %d characters", maxIdentifierPatternLength)
	}
	re, err := regexp.Compile(id.Pattern)
	if err != nil {
		return paramBad("identifier.pattern does not compile: %v", err)
	}
	if len(id.ExamplesValid) == 0 || len(id.ExamplesInvalid) == 0 || len(id.ExamplesValid) > maxExamples || len(id.ExamplesInvalid) > maxExamples {
		return paramBad("identifier needs 1 to %d examples_valid and 1 to %d examples_invalid: a pattern is proved by its own examples", maxExamples, maxExamples)
	}
	for _, e := range id.ExamplesValid {
		if len(e) > maxIdentifierLength || !re.MatchString(NormalizeIdentifier(id.Normalize, e)) {
			return paramBad("identifier example %q is declared valid but the pattern does not match it", e)
		}
	}
	for _, e := range id.ExamplesInvalid {
		if len(e) > maxIdentifierLength || re.MatchString(NormalizeIdentifier(id.Normalize, e)) {
			return paramBad("identifier example %q is declared invalid but the pattern matches it", e)
		}
	}
	return nil
}

func (l LicenceType) validate() error {
	if err := validateAuthority(l.Authority); err != nil {
		return err
	}
	if l.Term != nil && !l.Term.Valid() {
		return paramBad("licence.term needs years 0-100, months 0-11, days 0-365 and a positive total")
	}
	if err := validateDays("licence.renewal_notice_days", l.RenewalNoticeDays); err != nil {
		return err
	}
	if err := validateDays("licence.grace_days", l.GraceDays); err != nil {
		return err
	}
	return validateEvidenceList(l.EvidenceRequired)
}

func (e Eligibility) validate(label string) error {
	if !recordClassRe.MatchString(e.Metric) {
		return paramBad("%s.metric %q must be UPPER_SNAKE", label, e.Metric)
	}
	if !inList(operators, e.Operator) {
		return paramBad("%s.operator must be one of %s", label, strings.Join(operators, ", "))
	}
	if _, ok := parseDecimal(e.Threshold); !ok {
		return paramBad("%s.threshold %q must be a non-negative decimal string (numbers are refused: JUR-NEG-25)", label, e.Threshold)
	}
	if e.Currency != "" && !currencyRe.MatchString(e.Currency) {
		return paramBad("%s.currency must be a three-letter ISO 4217 code", label)
	}
	return nil
}

func (s SchemeType) validate() error {
	if err := validateAuthority(s.Authority); err != nil {
		return err
	}
	if err := s.Eligibility.validate("scheme.eligibility"); err != nil {
		return err
	}
	if s.LockIn != nil && !s.LockIn.Valid() {
		return paramBad("scheme.lock_in needs years 0-100, months 0-11, days 0-365 and a positive total")
	}
	if err := validateDays("scheme.cancellation_notice_days", s.CancellationNoticeDays); err != nil {
		return err
	}
	return validateEvidenceList(s.EvidenceRequired)
}

func daysInMonthNonLeap(m int) int {
	return []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m-1]
}

func (m ThresholdMonitor) validate() error {
	e := Eligibility{Metric: m.Metric, Operator: m.Operator, Threshold: m.Threshold, Currency: m.Currency}
	if err := e.validate("monitor"); err != nil {
		return err
	}
	if m.Operator != "GT" && m.Operator != "GTE" {
		return paramBad("monitor.operator must be GT or GTE: a monitor detects a threshold being reached")
	}
	if r, _ := parseDecimal(m.Threshold); r.Sign() == 0 {
		return paramBad("monitor.threshold must be positive")
	}
	switch m.Period.Kind {
	case periodRolling:
		if m.Period.Months < 1 || m.Period.Months > 60 || m.Period.StartMonth != 0 || m.Period.StartDay != 0 {
			return paramBad("a ROLLING period needs months 1 to 60 and no start_month or start_day")
		}
	case periodStatic:
		if m.Period.Months != 0 || m.Period.StartMonth < 1 || m.Period.StartMonth > 12 || m.Period.StartDay < 1 || m.Period.StartDay > daysInMonthNonLeap(m.Period.StartMonth) {
			return paramBad("a STATIC period needs a valid yearly start_month and start_day (29 February is not allowed) and no months")
		}
	default:
		return paramBad("monitor.period.kind must be STATIC or ROLLING")
	}
	if !inList(breachEffects, m.Breach.Effect) {
		return paramBad("monitor.breach.effect must be one of %s", strings.Join(breachEffects, ", "))
	}
	k := m.Breach.Effective.Kind
	if !inList(effectKinds, k) {
		return paramBad("monitor.breach.effective.kind must be one of %s", strings.Join(effectKinds, ", "))
	}
	if k == effectDaysAfter {
		if m.Breach.Effective.Days < 1 || m.Breach.Effective.Days > 365 {
			return paramBad("DAYS_AFTER needs days 1 to 365")
		}
	} else if m.Breach.Effective.Days != 0 {
		return paramBad("days belongs only to DAYS_AFTER")
	}
	if k == effectNextPeriodStart && m.Period.Kind != periodStatic {
		return paramBad("NEXT_PERIOD_START needs a STATIC period: a rolling window has no period start")
	}
	return nil
}

// pinnedRegistrationValue is the headline value a golden case must state (JUR-T030):
// the licence term (or its renewal notice), the scheme eligibility threshold, or the monitor
// threshold. A registration type is pinned by its own examples, checked when it is validated.
func pinnedRegistrationValue(p RuleParameters) string {
	switch p.Family {
	case FamilyLicenceType:
		if p.Licence != nil && p.Licence.Term != nil {
			return p.Licence.Term.ISO()
		}
		if p.Licence != nil {
			return fmt.Sprint(p.Licence.RenewalNoticeDays)
		}
	case FamilyScheme:
		if p.Scheme != nil {
			return p.Scheme.Eligibility.Threshold
		}
	case FamilyThresholdMonitor:
		if p.Monitor != nil {
			return p.Monitor.Threshold
		}
	}
	return ""
}

// ── runtime: identifier check ────────────────────────────────────────────────

// RegistrationResult is the explainable outcome of an identifier check. The identifier
// itself is never echoed or stored: only its digest, so the evidence ledger holds no tax identifiers.
type RegistrationResult struct {
	Outcome          string   `json:"outcome"`
	Kind             string   `json:"kind"`
	Authority        string   `json:"authority"`
	NormalizedDigest string   `json:"normalized_digest"`
	EvidenceRequired []string `json:"evidence_required,omitempty"`
	ThresholdMonitor string   `json:"threshold_monitor,omitempty"`
	Explanation      []string `json:"explanation"`
}

// IdentifierDigest is the sha256 of an identifier, as stored in evidence in place of the identifier.
func IdentifierDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

// CheckIdentifier tests an identifier's FORMAT against the registration type. It says nothing
// about whether the number is actually registered: only the authority can say that.
func CheckIdentifier(r RegistrationType, raw string) RegistrationResult {
	norm := NormalizeIdentifier(r.Identifier.Normalize, raw)
	res := RegistrationResult{Kind: r.Kind, Authority: r.Authority, NormalizedDigest: IdentifierDigest(norm),
		EvidenceRequired: r.EvidenceRequired, ThresholdMonitor: r.ThresholdMonitor, Explanation: []string{}}
	re := regexp.MustCompile(r.Identifier.Pattern) // validated when the rule was authored and again at compile
	if len(raw) > maxIdentifierLength || !re.MatchString(norm) {
		res.Outcome = OutcomeIdentifierInvalid
		res.Explanation = append(res.Explanation, fmt.Sprintf("the identifier does not match the %s format published for %s", r.Kind, r.Authority))
		return res
	}
	res.Outcome = OutcomeIdentifierValid
	res.Explanation = append(res.Explanation, fmt.Sprintf("the identifier matches the %s format for %s", r.Kind, r.Authority),
		"this is a format check only: whether the number is actually registered is for the authority to confirm")
	return res
}

// ── runtime: licence ─────────────────────────────────────────────────────────

// LicenceResult is the explainable outcome of a licence evaluation.
type LicenceResult struct {
	Outcome            string   `json:"outcome"`
	Status             string   `json:"status,omitempty"`
	IssueDate          string   `json:"issue_date"`
	ValidUntil         string   `json:"valid_until,omitempty"`
	RenewalWindowOpens string   `json:"renewal_window_opens,omitempty"`
	GraceEndsOn        string   `json:"grace_ends_on,omitempty"`
	DaysToExpiry       *int     `json:"days_to_expiry,omitempty"`
	EvidenceRequired   []string `json:"evidence_required,omitempty"`
	Explanation        []string `json:"explanation"`
}

func utcDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// EvaluateLicence derives a licence's validity from its issue date and the type's rules. A
// stated expiry is the last valid day, as printed on the licence; a derived one is the day before
// the term anniversary. A stated expiry beyond the maximum term is refused, not trusted.
func EvaluateLicence(l LicenceType, issue time.Time, expiry *time.Time, asOf time.Time) LicenceResult {
	issue, asOf = utcDay(issue), utcDay(asOf)
	res := LicenceResult{IssueDate: issue.Format(dayLayout), EvidenceRequired: l.EvidenceRequired, Explanation: []string{}}
	step := func(f string, a ...any) { res.Explanation = append(res.Explanation, fmt.Sprintf(f, a...)) }
	var until time.Time
	switch {
	case expiry != nil:
		until = utcDay(*expiry)
		if !until.After(issue) {
			res.Outcome = CalcInvalidFacts
			step("expiry_date %s is not after issue_date %s", until.Format(dayLayout), res.IssueDate)
			return res
		}
		if l.Term != nil {
			if max := l.Term.AddTo(issue).AddDate(0, 0, -1); until.After(max) {
				res.Outcome = LicenceTermExceeded
				step("the stated expiry %s is beyond the maximum term %s from issue (last valid day %s)", until.Format(dayLayout), l.Term.ISO(), max.Format(dayLayout))
				return res
			}
		}
		step("expiry is the stated last valid day, %s", until.Format(dayLayout))
	case l.Term != nil:
		until = l.Term.AddTo(issue).AddDate(0, 0, -1)
		step("term %s from issue %s: last valid day %s", l.Term.ISO(), res.IssueDate, until.Format(dayLayout))
	default:
		res.Outcome = CalcInvalidFacts
		step("this licence type has no fixed term: expiry_date must be supplied from the licence itself")
		return res
	}
	res.Outcome = OutcomeLicenceEvaluated
	res.ValidUntil = until.Format(dayLayout)
	opens := until.AddDate(0, 0, -l.RenewalNoticeDays)
	graceEnd := until.AddDate(0, 0, l.GraceDays)
	if l.RenewalNoticeDays > 0 {
		res.RenewalWindowOpens = opens.Format(dayLayout)
	}
	if l.GraceDays > 0 {
		res.GraceEndsOn = graceEnd.Format(dayLayout)
	}
	days := int(until.Sub(asOf).Hours() / 24)
	res.DaysToExpiry = &days
	switch {
	case asOf.Before(issue):
		res.Status = LicenceNotYetValid
		step("as of %s the licence has not been issued", asOf.Format(dayLayout))
	case !asOf.After(until) && l.RenewalNoticeDays > 0 && !asOf.Before(opens):
		res.Status = LicenceRenewalWindow
		step("as of %s the renewal window (opened %s) is open: %d days remain", asOf.Format(dayLayout), opens.Format(dayLayout), days)
	case !asOf.After(until):
		res.Status = LicenceActive
		step("as of %s the licence is valid for %d more days", asOf.Format(dayLayout), days)
	case l.GraceDays > 0 && !asOf.After(graceEnd):
		res.Status = LicenceInGrace
		step("as of %s the licence has expired but is inside the %d-day grace period ending %s", asOf.Format(dayLayout), l.GraceDays, graceEnd.Format(dayLayout))
	default:
		res.Status = LicenceExpired
		step("as of %s the licence expired on %s", asOf.Format(dayLayout), until.Format(dayLayout))
	}
	return res
}

// ── runtime: scheme / election ───────────────────────────────────────────────

// SchemeResult is the explainable outcome of a scheme eligibility and election check.
type SchemeResult struct {
	Outcome              string   `json:"outcome"`
	Metric               string   `json:"metric"`
	MetricValue          string   `json:"metric_value"`
	Comparison           string   `json:"comparison"`
	StartDate            string   `json:"start_date"`
	LockInEnds           string   `json:"lock_in_ends,omitempty"`
	CancelRequested      string   `json:"cancel_requested,omitempty"`
	EarliestCancellation string   `json:"earliest_cancellation_effective,omitempty"`
	DeferredByLockIn     bool     `json:"deferred_by_lock_in,omitempty"`
	EvidenceRequired     []string `json:"evidence_required,omitempty"`
	Explanation          []string `json:"explanation"`
}

func compareMetric(op string, v, t *big.Rat) bool {
	c := v.Cmp(t)
	switch op {
	case "LT":
		return c < 0
	case "LTE":
		return c <= 0
	case "GT":
		return c > 0
	}
	return c >= 0
}

// EvaluateScheme checks eligibility on the metric and, when a cancellation is asked for, when it can
// take effect: never before the lock-in ends, and never sooner than the notice period.
func EvaluateScheme(s SchemeType, metricValue string, start time.Time, cancel *time.Time) SchemeResult {
	start = utcDay(start)
	res := SchemeResult{Metric: s.Eligibility.Metric, MetricValue: metricValue, StartDate: start.Format(dayLayout),
		Comparison:       fmt.Sprintf("%s %s %s", s.Eligibility.Metric, s.Eligibility.Operator, s.Eligibility.Threshold),
		EvidenceRequired: s.EvidenceRequired, Explanation: []string{}}
	step := func(f string, a ...any) { res.Explanation = append(res.Explanation, fmt.Sprintf(f, a...)) }
	v, ok := parseDecimal(metricValue)
	if !ok {
		res.Outcome = CalcInvalidFacts
		step("metric_value %q must be a non-negative decimal string", metricValue)
		return res
	}
	thr, _ := parseDecimal(s.Eligibility.Threshold)
	if !compareMetric(s.Eligibility.Operator, v, thr) {
		res.Outcome = OutcomeSchemeNotEligible
		step("not eligible: %s is %s but the scheme requires %s %s", s.Eligibility.Metric, metricValue, s.Eligibility.Operator, s.Eligibility.Threshold)
		return res
	}
	res.Outcome = OutcomeSchemeEligible
	step("eligible: %s %s satisfies %s %s", s.Eligibility.Metric, metricValue, s.Eligibility.Operator, s.Eligibility.Threshold)
	lockEnd := start
	if s.LockIn != nil {
		lockEnd = s.LockIn.AddTo(start)
		res.LockInEnds = lockEnd.Format(dayLayout)
		step("an election starting %s is locked in until %s (%s)", res.StartDate, res.LockInEnds, s.LockIn.ISO())
	}
	if cancel != nil {
		c := utcDay(*cancel)
		res.CancelRequested = c.Format(dayLayout)
		eff := c.AddDate(0, 0, s.CancellationNoticeDays)
		if eff.Before(lockEnd) {
			eff, res.DeferredByLockIn = lockEnd, true
			step("a cancellation asked for %s with %d days' notice would fall inside the lock-in: it takes effect when the lock-in ends", res.CancelRequested, s.CancellationNoticeDays)
		}
		res.EarliestCancellation = eff.Format(dayLayout)
		step("the cancellation takes effect no earlier than %s", res.EarliestCancellation)
	}
	return res
}

// ── runtime: threshold monitor ───────────────────────────────────────────────

// Observation is one dated amount of the monitored metric.
type Observation struct {
	Date   string `json:"date"`
	Amount string `json:"amount"`
}

// ThresholdResult is the explainable outcome of a threshold evaluation.
type ThresholdResult struct {
	Outcome       string `json:"outcome"`
	Metric        string `json:"metric"`
	Breached      bool   `json:"breached"`
	Effect        string `json:"effect,omitempty"`
	BreachDate    string `json:"breach_date,omitempty"`
	EffectiveDate string `json:"effective_date,omitempty"`
	// RegistrationEffectiveDate is set only for a REGISTRATION_REQUIRED breach: it is the value the obligation
	// engine takes as the registration_date fact (s15).
	RegistrationEffectiveDate string   `json:"registration_effective_date,omitempty"`
	TotalAtBreach             string   `json:"total_at_breach,omitempty"`
	Total                     string   `json:"total"`
	Threshold                 string   `json:"threshold"`
	Headroom                  string   `json:"headroom,omitempty"`
	WindowFrom                string   `json:"window_from"`
	WindowTo                  string   `json:"window_to"`
	Considered                int      `json:"observations_considered"`
	IgnoredAfterAsOf          int      `json:"observations_after_as_of_ignored,omitempty"`
	Explanation               []string `json:"explanation"`
}

// addMonthsClamp adds n (possibly negative) months, clamping to the end of the target month
// (31 Mar minus one month is 28 or 29 Feb, never 3 Mar).
func addMonthsClamp(t time.Time, n int) time.Time {
	t = utcDay(t)
	m := int(t.Month()) - 1 + n
	y := t.Year() + floorDiv(m, 12)
	mo := time.Month(floorMod(m, 12) + 1)
	last := time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day()
	d := t.Day()
	if d > last {
		d = last
	}
	return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func floorMod(a, b int) int { return a - floorDiv(a, b)*b }

// periodStartOn is the start of the STATIC period containing day t.
func periodStartOn(p MonitorPeriod, t time.Time) time.Time {
	s := time.Date(t.Year(), time.Month(p.StartMonth), p.StartDay, 0, 0, 0, 0, time.UTC)
	if s.After(t) {
		s = s.AddDate(-1, 0, 0)
	}
	return s
}

type parsedObs struct {
	day time.Time
	amt *big.Rat
}

// EvaluateThreshold sums the observations over the monitor's period and reports whether, and from which
// date, the threshold was met, and when that takes effect. Amounts are exact (rational) and never floating
// point. Observations dated after as_of are ignored, so a re-run for an earlier date reproduces the earlier answer.
func EvaluateThreshold(m ThresholdMonitor, obs []Observation, asOf time.Time) ThresholdResult {
	asOf = utcDay(asOf)
	res := ThresholdResult{Metric: m.Metric, Threshold: m.Threshold, Total: "0", Explanation: []string{}}
	step := func(f string, a ...any) { res.Explanation = append(res.Explanation, fmt.Sprintf(f, a...)) }
	invalid := func(f string, a ...any) ThresholdResult {
		res.Outcome = CalcInvalidFacts
		step(f, a...)
		return res
	}
	if len(obs) > maxObservations {
		return invalid("at most %d observations are accepted", maxObservations)
	}
	var all []parsedObs
	for i, o := range obs {
		d, err := time.Parse(dayLayout, o.Date)
		if err != nil {
			return invalid("observations[%d].date %q must be a date, YYYY-MM-DD", i, o.Date)
		}
		a, ok := parseDecimal(o.Amount)
		if !ok {
			return invalid("observations[%d].amount %q must be a non-negative decimal string", i, o.Amount)
		}
		if d.After(asOf) {
			res.IgnoredAfterAsOf++
			continue
		}
		all = append(all, parsedObs{d, a})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].day.Before(all[j].day) })
	thr, _ := parseDecimal(m.Threshold)

	// inWindow reports whether an observation counts toward the total measured at day end.
	var from func(end time.Time) time.Time // first day counted, inclusive
	if m.Period.Kind == periodStatic {
		from = func(end time.Time) time.Time { return periodStartOn(m.Period, end) }
	} else {
		from = func(end time.Time) time.Time { return addMonthsClamp(end, -m.Period.Months).AddDate(0, 0, 1) }
	}
	totalAt := func(end time.Time) *big.Rat {
		sum, lo := new(big.Rat), from(end)
		for _, o := range all {
			if !o.day.Before(lo) && !o.day.After(end) {
				sum.Add(sum, o.amt)
			}
		}
		return sum
	}
	lo := from(asOf)
	res.WindowFrom, res.WindowTo = lo.Format(dayLayout), asOf.Format(dayLayout)
	for _, o := range all {
		if !o.day.Before(lo) {
			res.Considered++
		}
	}
	total := totalAt(asOf)
	res.Total = decOut(total)
	step("%s over %s to %s: total %s against %s %s", m.Metric, res.WindowFrom, res.WindowTo, res.Total, m.Operator, m.Threshold)

	res.Outcome = OutcomeThresholdEvaluated
	var breach time.Time
	var atBreach *big.Rat
	for _, o := range all {
		if o.day.Before(lo) {
			continue
		}
		if t := totalAt(o.day); compareMetric(m.Operator, t, thr) {
			breach, atBreach = o.day, t
			break
		}
	}
	if breach.IsZero() {
		if left := new(big.Rat).Sub(thr, total); left.Sign() > 0 {
			res.Headroom = decOut(left)
		}
		step("the threshold has not been reached in this window")
		return res
	}
	res.Breached, res.Effect = true, m.Breach.Effect
	res.BreachDate, res.TotalAtBreach = breach.Format(dayLayout), decOut(atBreach)
	step("the threshold was first reached on %s (total %s)", res.BreachDate, res.TotalAtBreach)
	var eff time.Time
	switch m.Breach.Effective.Kind {
	case effectImmediate:
		eff = breach
	case effectNextPeriodStart:
		eff = periodStartOn(m.Period, breach).AddDate(1, 0, 0)
	case effectDaysAfter:
		eff = breach.AddDate(0, 0, m.Breach.Effective.Days)
	default:
		eff = time.Date(breach.Year(), breach.Month()+2, 0, 0, 0, 0, 0, time.UTC)
	}
	res.EffectiveDate = eff.Format(dayLayout)
	step("effect %s takes legal effect on %s (%s)", m.Breach.Effect, res.EffectiveDate, m.Breach.Effective.Kind)
	if m.Breach.Effect == breachRegistration {
		res.RegistrationEffectiveDate = res.EffectiveDate
		step("registration is required from %s; pass it to the obligation engine as registration_date", res.EffectiveDate)
	}
	return res
}

// decOut renders an exact amount with up to 12 decimals and no trailing zeros ("90000", "0.5").
func decOut(r *big.Rat) string {
	s := r.FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
