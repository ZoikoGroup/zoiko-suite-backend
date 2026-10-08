package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ZS-JUR-001 Wave 6: retention rules (s18) and accounting and reporting
// mappings (s19), as two more closed parameter families on the governed rule
// module (see jurisdiction-pack-decision-rule-parameters.md). They inherit
// provenance, independent publication, signing, certification and rollout.
//
//	RETENTION {"family":"RETENTION","record_class":"PAYROLL_RECORDS","trigger":"EMPLOYMENT_END",
//	           "minimum":{"years":6,"months":0,"days":0},"format_requirement":"...",
//	           "legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}
//	MAPPING   {"family":"MAPPING","mapping_type":"TAX_CODE_TO_ACCOUNT_CLASS","entries":[{"from":"VAT_STD","to":"OUTPUT_TAX"}]}
//
// This service never deletes a record and never posts to a ledger: a retention
// rule says when destruction is ELIGIBLE (a legal hold always overrides), and a
// mapping says which class or line a code belongs to. The Accounting Kernel
// stays the only posting authority.

const (
	FamilyRetention = "RETENTION"
	FamilyMapping   = "MAPPING"

	RetentionDomain = "RECORDS_RETENTION"
	MappingDomain   = "ACCOUNTING_MAPPING"

	DestructionEligibilityOnly = "ELIGIBILITY_ONLY"

	OutcomeRetentionResolved = "RETENTION_RESOLVED"
	RetentionTenantTooShort  = "TENANT_POLICY_TOO_SHORT" // JUR-NEG-12
	RetentionTriggerMismatch = "TRIGGER_MISMATCH"
	OutcomeMappingResolved   = "MAPPING_RESOLVED"
	MappingEntryNotFound     = "MAPPING_ENTRY_NOT_FOUND"

	maxMappingEntries = 500
)

// RetentionTriggers are the events a retention period can run from (s18).
var RetentionTriggers = []string{"CREATION", "PERIOD_END", "FILING_DATE", "CONTRACT_END", "EMPLOYMENT_END", "EVENT"}

// MappingTypes are the s19 mapping kinds.
var MappingTypes = []string{"TAX_CODE_TO_ACCOUNT_CLASS", "STATUTORY_LINE_TO_METRIC", "REPORTING_TAXONOMY", "DISCLOSURE_REQUIREMENT", "CONTROL_ACCOUNT_CLASS"}

// Duration is a retention period.
type Duration struct {
	Years  int `json:"years"`
	Months int `json:"months"`
	Days   int `json:"days"`
}

// MappingEntry maps one code to another.
type MappingEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

var (
	recordClassRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	mappingCodeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:\-]{0,99}$`)
)

func inList(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Valid reports whether the duration is in range and positive.
func (d Duration) Valid() bool {
	return d.Years >= 0 && d.Years <= 100 && d.Months >= 0 && d.Months <= 11 && d.Days >= 0 && d.Days <= 365 && (d.Years+d.Months+d.Days) > 0
}

// ISO renders P6Y, P1Y6M, P90D.
func (d Duration) ISO() string {
	var b strings.Builder
	b.WriteString("P")
	if d.Years > 0 {
		b.WriteString(strconv.Itoa(d.Years) + "Y")
	}
	if d.Months > 0 {
		b.WriteString(strconv.Itoa(d.Months) + "M")
	}
	if d.Days > 0 {
		b.WriteString(strconv.Itoa(d.Days) + "D")
	}
	return b.String()
}

// AddTo applies the duration to a calendar date: months clamp to the end of the
// target month (31 Jan + 1 month = 28 or 29 Feb), then days are added.
func (d Duration) AddTo(t time.Time) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	m := int(t.Month()) - 1 + d.Years*12 + d.Months
	y := t.Year() + m/12
	mo := time.Month(m%12 + 1)
	last := time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day()
	day := t.Day()
	if day > last {
		day = last
	}
	return time.Date(y, mo, day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, d.Days)
}

func (p RuleParameters) validateRetention() error {
	if p.Class != "" || p.Rate != nil || len(p.Bands) != 0 || p.Amount != nil || p.Unit != "" || p.Rounding.Mode != "" || p.Rounding.Scale != 0 ||
		p.MappingType != "" || len(p.Entries) != 0 || p.Profile != nil {
		return paramBad("RETENTION takes only record_class, trigger, minimum, format_requirement, legal_hold_override and destruction_rule")
	}
	if !recordClassRe.MatchString(p.RecordClass) {
		return paramBad("record_class %q must be UPPER_SNAKE", p.RecordClass)
	}
	if !inList(RetentionTriggers, p.Trigger) {
		return paramBad("trigger must be one of %s", strings.Join(RetentionTriggers, ", "))
	}
	if p.Minimum == nil || !p.Minimum.Valid() {
		return paramBad("minimum needs years 0-100, months 0-11, days 0-365, and a positive total")
	}
	if len(p.FormatRequirement) > 200 {
		return paramBad("format_requirement is too long")
	}
	if p.LegalHoldOverride == nil || !*p.LegalHoldOverride {
		return paramBad("legal_hold_override must be true: a legal hold always overrides destruction eligibility (s18)")
	}
	if p.DestructionRule != DestructionEligibilityOnly {
		return paramBad("destruction_rule must be %s: this service decides eligibility, it never deletes", DestructionEligibilityOnly)
	}
	return nil
}

func (p RuleParameters) validateMapping() error {
	if p.Class != "" || p.Rate != nil || len(p.Bands) != 0 || p.Amount != nil || p.Unit != "" || p.Rounding.Mode != "" || p.Rounding.Scale != 0 ||
		p.RecordClass != "" || p.Trigger != "" || p.Minimum != nil || p.FormatRequirement != "" || p.LegalHoldOverride != nil || p.DestructionRule != "" || p.Profile != nil {
		return paramBad("MAPPING takes only mapping_type and entries")
	}
	if !inList(MappingTypes, p.MappingType) {
		return paramBad("mapping_type must be one of %s", strings.Join(MappingTypes, ", "))
	}
	if len(p.Entries) == 0 || len(p.Entries) > maxMappingEntries {
		return paramBad("entries needs 1 to %d items", maxMappingEntries)
	}
	seen := map[string]bool{}
	for i, e := range p.Entries {
		if !mappingCodeRe.MatchString(e.From) || !mappingCodeRe.MatchString(e.To) {
			return paramBad("entries[%d]: from and to must be 1-100 characters of letters, digits and _ . : -", i)
		}
		if len(e.Note) > 300 {
			return paramBad("entries[%d]: note is too long", i)
		}
		if seen[e.From] {
			return paramBad("entries[%d]: %q is mapped twice; a mapping must be unambiguous", i, e.From)
		}
		seen[e.From] = true
	}
	return nil
}

// ParameterOnlyFamily reports families that carry values for a consumer to use
// and are never calculated with.
func ParameterOnlyFamily(f string) bool {
	return f == FamilyAmount || f == FamilyRetention || f == FamilyMapping || f == FamilyEInvoiceProfile || f == FamilyFilingProfile
}

// PinnedParameterValue is the human-checkable value a golden case must state for
// a parameter-only rule (JUR-T030): the amount, the ISO retention duration, or the
// number of mapping entries.
func PinnedParameterValue(p RuleParameters) string {
	switch p.Family {
	case FamilyAmount:
		if p.Amount != nil {
			return *p.Amount
		}
	case FamilyRetention:
		if p.Minimum != nil {
			return p.Minimum.ISO()
		}
	case FamilyMapping:
		return strconv.Itoa(len(p.Entries))
	case FamilyEInvoiceProfile, FamilyFilingProfile:
		if p.Profile != nil {
			return p.Profile.ProfileVersion
		}
	}
	return ""
}

// RetentionResult is the explainable outcome of a retention determination.
type RetentionResult struct {
	Outcome            string   `json:"outcome"`
	RecordClass        string   `json:"record_class"`
	Trigger            string   `json:"trigger"`
	TriggerDate        string   `json:"trigger_date"`
	StatutoryMinimum   string   `json:"statutory_minimum"`
	RetainUntil        string   `json:"retain_until"`
	FormatRequirement  string   `json:"format_requirement,omitempty"`
	LegalHoldOverride  bool     `json:"legal_hold_override"`
	DestructionRule    string   `json:"destruction_rule"`
	TenantMinimum      string   `json:"tenant_minimum,omitempty"`
	TenantRetainUntil  string   `json:"tenant_retain_until,omitempty"`
	EffectiveRetention string   `json:"effective_retain_until,omitempty"`
	Explanation        []string `json:"explanation"`
}

// DetermineRetention applies a retention rule to a trigger date. A tenant policy
// may EXTEND retention but never shorten it below the statutory minimum (JUR-NEG-12).
// requestedTrigger, when set, must match the rule's trigger event.
func DetermineRetention(p RuleParameters, triggerDate time.Time, requestedTrigger string, tenant *Duration) RetentionResult {
	res := RetentionResult{RecordClass: p.RecordClass, Trigger: p.Trigger, TriggerDate: triggerDate.Format("2006-01-02"),
		LegalHoldOverride: true, DestructionRule: DestructionEligibilityOnly, FormatRequirement: p.FormatRequirement, Explanation: []string{}}
	step := func(f string, a ...any) { res.Explanation = append(res.Explanation, fmt.Sprintf(f, a...)) }
	if p.Minimum == nil {
		res.Outcome = CalcNoParameters
		return res
	}
	if requestedTrigger != "" && requestedTrigger != p.Trigger {
		res.Outcome = RetentionTriggerMismatch
		step("the statutory period for %s runs from %s, not %s", p.RecordClass, p.Trigger, requestedTrigger)
		return res
	}
	res.StatutoryMinimum = p.Minimum.ISO()
	res.RetainUntil = p.Minimum.AddTo(triggerDate).Format("2006-01-02")
	res.EffectiveRetention = res.RetainUntil
	step("statutory minimum %s from %s %s: retain until %s", res.StatutoryMinimum, p.Trigger, res.TriggerDate, res.RetainUntil)
	step("destruction is ELIGIBILITY ONLY after that date and a legal hold always overrides; this service deletes nothing")
	res.Outcome = OutcomeRetentionResolved
	if tenant != nil {
		res.TenantMinimum = tenant.ISO()
		tEnd := tenant.AddTo(triggerDate)
		res.TenantRetainUntil = tEnd.Format("2006-01-02")
		if p.Minimum.AddTo(triggerDate).After(tEnd) {
			res.Outcome = RetentionTenantTooShort
			step("tenant policy %s ends %s, before the statutory %s: refused, a tenant may extend retention but never shorten it", res.TenantMinimum, res.TenantRetainUntil, res.RetainUntil)
			return res
		}
		res.EffectiveRetention = res.TenantRetainUntil
		step("tenant policy %s is at least the statutory minimum: effective retention until %s", res.TenantMinimum, res.EffectiveRetention)
	}
	return res
}

// MappingResult is the explainable outcome of a mapping lookup.
type MappingResult struct {
	Outcome     string         `json:"outcome"`
	MappingType string         `json:"mapping_type"`
	From        string         `json:"from,omitempty"`
	To          string         `json:"to,omitempty"`
	Note        string         `json:"note,omitempty"`
	Entries     []MappingEntry `json:"entries,omitempty"`
	Explanation []string       `json:"explanation"`
}

// LookupMapping answers one code, or returns the whole mapping when from is empty.
func LookupMapping(p RuleParameters, from string) MappingResult {
	res := MappingResult{MappingType: p.MappingType, Explanation: []string{}}
	if from == "" {
		res.Outcome, res.Entries = OutcomeMappingResolved, p.Entries
		res.Explanation = append(res.Explanation, fmt.Sprintf("%d entries of %s", len(p.Entries), p.MappingType))
		return res
	}
	res.From = from
	for _, e := range p.Entries {
		if e.From == from {
			res.Outcome, res.To, res.Note = OutcomeMappingResolved, e.To, e.Note
			res.Explanation = append(res.Explanation, fmt.Sprintf("%s maps to %s (%s); a mapping classifies, it never posts to the ledger", from, e.To, p.MappingType))
			return res
		}
	}
	res.Outcome = MappingEntryNotFound
	res.Explanation = append(res.Explanation, fmt.Sprintf("%s has no entry in this mapping: nothing is guessed", from))
	return res
}

// isoDurationRe matches the retention durations Duration.ISO produces (P6Y, P1Y6M, P90D).
var isoDurationRe = regexp.MustCompile(`^P([0-9]{1,3}Y)?([0-9]{1,2}M)?([0-9]{1,3}D)?$`)

var reservedDomains = []string{RetentionDomain, MappingDomain, EInvoiceDomain, FilingDomain}

// ReservedDomain reports rule domains that hold only one parameter family.
func ReservedDomain(d string) bool { return inList(reservedDomains, d) }

// FamilyDomain is the one rule domain a family may live in, or "" for families
// that are not tied to a domain (the tax and amount families).
func FamilyDomain(family string) string {
	switch family {
	case FamilyRetention:
		return RetentionDomain
	case FamilyMapping:
		return MappingDomain
	case FamilyEInvoiceProfile:
		return EInvoiceDomain
	case FamilyFilingProfile:
		return FilingDomain
	}
	return ""
}
