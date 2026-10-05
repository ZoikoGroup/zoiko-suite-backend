package domain

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Communication intents (ZS-SVC-Y-001 NCD-01 sections 4.1 to 4.3, 9.2).
//
// An intent is WHY a message exists. It owns the facts that decide how the message may
// be treated, so they are governed and versioned rather than chosen by whoever sends:
// the purpose class, the evidence the delivery must leave, the channels it may use,
// whether promotional content is allowed, and a typed contract for the variables it
// may be given. The wording lives in templates, bound to an intent.

// Intent errors. NCD-nnn are the stable reason codes of section 10.3.
const (
	ErrIntentNotFound             = errorString("NCD-001 INTENT_NOT_FOUND: no such communication intent")
	ErrIntentNotEffective         = errorString("NCD-002 INTENT_NOT_EFFECTIVE: the intent has no version in force at that time")
	ErrIntentVersionNotFound      = errorString("communication intent version not found")
	ErrIntentKeyTaken             = errorString("an intent with this key already exists for the tenant")
	ErrIntentRetired              = errorString("the intent is retired and takes no new versions")
	ErrIntentVersionNotDraft      = errorString("only a DRAFT intent version can be validated")
	ErrIntentVersionNotReview     = errorString("only a REVIEW intent version can be approved")
	ErrIntentVersionNotApproved   = errorString("only an APPROVED intent version can be published")
	ErrIntentVersionSelfApproval  = errorString("the principal who created an intent version cannot approve it")
	ErrIntentInvalid              = errorString("the communication intent is invalid")
	ErrIntentEffectiveFromInvalid = errorString("effective_from cannot be in the past, and must be after every version already published")
	ErrTemplateVariableInvalid    = errorString("NCD-004 TEMPLATE_VARIABLE_INVALID: a variable does not satisfy the intent's contract")
)

// IntentProblem explains why an intent or its contract was refused; it matches
// ErrIntentInvalid.
type IntentProblem struct{ Reason string }

func (e IntentProblem) Error() string        { return "communication intent is invalid: " + e.Reason }
func (e IntentProblem) Is(target error) bool { return target == ErrIntentInvalid }

// VariableProblem explains which variables broke the contract; it matches
// ErrTemplateVariableInvalid.
type VariableProblem struct{ Problems []string }

func (e VariableProblem) Error() string {
	return "NCD-004 TEMPLATE_VARIABLE_INVALID: " + strings.Join(e.Problems, "; ")
}
func (e VariableProblem) Is(target error) bool { return target == ErrTemplateVariableInvalid }

// Vocabularies.
var (
	// PurposeClasses are the classes the policy engine already judges.
	PurposeClasses = []string{"S0", "T0", "A1", "L1", "M1"}
	// EvidenceClasses E0 (best effort) to E4 (formal acknowledgment package). Their exact
	// minimum requirements per channel are an open decision (OD-06); they are recorded
	// and carried, not yet enforced against a provider's capability.
	EvidenceClasses = []string{"E0", "E1", "E2", "E3", "E4"}
	// IntentChannels are the channels an intent may allow. The platform delivers EMAIL and
	// IN_APP today; SMS and PUSH are accepted in a contract so it can be written once.
	IntentChannels = []string{"EMAIL", "IN_APP", "SMS", "PUSH"}
	// VariableTypes and Sensitivities of the typed variable contract.
	VariableTypes = []string{"STRING", "NUMBER", "DATE", "ID", "URL"}
	Sensitivities = []string{"S0", "S1", "S2", "S3"}
)

const (
	maxContractVariables   = 50
	defaultVariableLength  = 200
	maxVariableLength      = 2000
	subjectSafeSensitivity = "S1" // S0 and S1 may appear in a subject; S2 and S3 never (INV-17)
)

var (
	intentKeyRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	variableNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	numberValueRe   = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,14})(\.[0-9]{1,12})?$`)
	idValueRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	intentVersionLn = 120
)

// CommunicationIntent is the stable identity of a purpose.
type CommunicationIntent struct {
	IntentID             string     `json:"intent_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	IntentKey            string     `json:"intent_key"`
	DisplayName          string     `json:"display_name"`
	DomainOwner          string     `json:"domain_owner"`
	Status               string     `json:"status"` // ACTIVE, RETIRED
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	CreatedAt            time.Time  `json:"created_at"`
	RetiredAt            *time.Time `json:"retired_at,omitempty"`
}

// VariableSpec is one entry of the typed variable contract.
type VariableSpec struct {
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Sensitivity string `json:"sensitivity"`
	MaxLength   int    `json:"max_length,omitempty"`
}

// IntentVersion is the governed content of an intent at one point in its history.
type IntentVersion struct {
	VersionID         string                  `json:"version_id"`
	IntentID          string                  `json:"intent_id"`
	TenantID          string                  `json:"tenant_id"`
	LegalEntityID     string                  `json:"legal_entity_id"`
	VersionNumber     int                     `json:"version_number"`
	PurposeClass      string                  `json:"purpose_class"`
	EvidenceClass     string                  `json:"evidence_class"`
	AllowedChannels   []string                `json:"allowed_channels"`
	MarketingAllowed  bool                    `json:"marketing_allowed"`
	RecordRequirement bool                    `json:"record_requirement"`
	VariableContract  map[string]VariableSpec `json:"variable_contract"`
	Status            string                  `json:"status"` // DRAFT, REVIEW, APPROVED, PUBLISHED

	// PrivacyActivityID and PrivacyPurposeID name the privacy processing activity and purpose
	// this intent's messages run under (both or neither); the privacy gate asks the decision
	// service about exactly these. Frozen with the version.
	PrivacyActivityID *string `json:"privacy_activity_id,omitempty"`
	PrivacyPurposeID  *string `json:"privacy_purpose_id,omitempty"`

	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	CreatedAt              time.Time  `json:"created_at"`
	ValidatedAt            *time.Time `json:"validated_at,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	EffectiveFrom          *time.Time `json:"effective_from,omitempty"`
	PublishedAt            *time.Time `json:"published_at,omitempty"`
	PublishedByPrincipalID *string    `json:"published_by_principal_id,omitempty"`
}

// Intent version lifecycle.
const (
	IntentVersionDraft     = "DRAFT"
	IntentVersionReview    = "REVIEW"
	IntentVersionApproved  = "APPROVED"
	IntentVersionPublished = "PUBLISHED"
)

// CreateIntentParams creates the stable identity.
type CreateIntentParams struct {
	LegalEntityID, IntentKey, DisplayName, DomainOwner, CreatedByPrincipalID string
}

// CreateIntentVersionParams writes a new DRAFT version.
type CreateIntentVersionParams struct {
	IntentID             string
	PurposeClass         string
	EvidenceClass        string
	AllowedChannels      []string
	MarketingAllowed     bool
	RecordRequirement    bool
	VariableContract     map[string]VariableSpec
	PrivacyActivityID    *string
	PrivacyPurposeID     *string
	CreatedByPrincipalID string
}

// ApproveIntentVersionParams approves a REVIEW version.
type ApproveIntentVersionParams struct{ VersionID, ApprovedByPrincipalID string }

// PublishIntentVersionParams publishes an APPROVED version. EffectiveFrom nil means now.
type PublishIntentVersionParams struct {
	VersionID, PublishedByPrincipalID string
	EffectiveFrom                     *time.Time
}

func inSet(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ValidateIntentKey checks a stable intent key such as payroll.payslip_available.
func ValidateIntentKey(key string) error {
	if len(key) > intentVersionLn || !intentKeyRe.MatchString(key) {
		return IntentProblem{"intent_key must be dot-separated lower-case words (letters, digits, underscore), for example payroll.payslip_available"}
	}
	return nil
}

// NormalizeContract fills defaults (the maximum length of a STRING) and returns a copy.
func NormalizeContract(c map[string]VariableSpec) map[string]VariableSpec {
	out := make(map[string]VariableSpec, len(c))
	for name, s := range c {
		if s.Type == "STRING" && s.MaxLength == 0 {
			s.MaxLength = defaultVariableLength
		}
		out[name] = s
	}
	return out
}

// ValidateContract checks a typed variable contract. A variable always declares its
// type and its sensitivity: nothing is guessed, so the author has to have thought about
// both.
func ValidateContract(c map[string]VariableSpec) error {
	if len(c) > maxContractVariables {
		return IntentProblem{fmt.Sprintf("a contract declares at most %d variables", maxContractVariables)}
	}
	names := make([]string, 0, len(c))
	for n := range c {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s := c[name]
		if !variableNameRe.MatchString(name) {
			return IntentProblem{fmt.Sprintf("variable name %q must be lower-case letters, digits and underscores", name)}
		}
		if !inSet(VariableTypes, s.Type) {
			return IntentProblem{fmt.Sprintf("variable %q: type must be one of %s", name, strings.Join(VariableTypes, ", "))}
		}
		if !inSet(Sensitivities, s.Sensitivity) {
			return IntentProblem{fmt.Sprintf("variable %q: sensitivity must be one of %s (an author states it, it is never defaulted)", name, strings.Join(Sensitivities, ", "))}
		}
		if s.Type != "STRING" && s.MaxLength != 0 {
			return IntentProblem{fmt.Sprintf("variable %q: max_length only applies to STRING", name)}
		}
		if s.MaxLength < 0 || s.MaxLength > maxVariableLength {
			return IntentProblem{fmt.Sprintf("variable %q: max_length is 1 to %d", name, maxVariableLength)}
		}
	}
	return nil
}

// ValidateIntentVersion checks everything about a version that can be known without
// storing it.
func ValidateIntentVersion(p CreateIntentVersionParams) error {
	if !inSet(PurposeClasses, p.PurposeClass) {
		return IntentProblem{"purpose_class must be one of " + strings.Join(PurposeClasses, ", ")}
	}
	if !inSet(EvidenceClasses, p.EvidenceClass) {
		return IntentProblem{"evidence_class must be one of " + strings.Join(EvidenceClasses, ", ")}
	}
	if len(p.AllowedChannels) == 0 || len(p.AllowedChannels) > len(IntentChannels) {
		return IntentProblem{"allowed_channels needs 1 to " + fmt.Sprint(len(IntentChannels)) + " channels"}
	}
	seen := map[string]bool{}
	for _, ch := range p.AllowedChannels {
		if !inSet(IntentChannels, ch) || seen[ch] {
			return IntentProblem{"allowed_channels must be distinct values of " + strings.Join(IntentChannels, ", ")}
		}
		seen[ch] = true
	}
	// Marketing content in a security, transactional or operational message is how a
	// promotion gets past an opt-out (INV-07, 11.4): only lifecycle and marketing
	// purposes may ever allow it.
	if p.MarketingAllowed && p.PurposeClass != "L1" && p.PurposeClass != "M1" {
		return IntentProblem{"marketing_allowed is only possible for a lifecycle (L1) or marketing (M1) purpose"}
	}
	if (p.PrivacyActivityID == nil) != (p.PrivacyPurposeID == nil) {
		return IntentProblem{"privacy_activity_id and privacy_purpose_id are given together or not at all"}
	}
	for _, id := range []*string{p.PrivacyActivityID, p.PrivacyPurposeID} {
		if id != nil && !privacyIDRe.MatchString(*id) {
			return IntentProblem{"privacy_activity_id and privacy_purpose_id must start with a letter or digit and use only letters, digits and _ . : - (at most 128)"}
		}
	}
	return ValidateContract(p.VariableContract)
}

var privacyIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// SubjectSafe reports whether a variable may appear in a subject line: S0 and S1 only.
func SubjectSafe(s VariableSpec) bool { return s.Sensitivity == "S0" || s.Sensitivity == "S1" }

// CheckSubjectAgainstContract replaces the author's say-so with the contract: every
// variable a subject uses must exist in the contract and be subject-safe, so a salary
// or a disciplinary reason (S2, S3) can never reach a subject (INV-17, NP-32).
func CheckSubjectAgainstContract(subjectVariables []string, c map[string]VariableSpec) error {
	for _, v := range subjectVariables {
		s, ok := c[v]
		if !ok {
			return SubjectProblem{fmt.Sprintf("the subject uses %q, which the intent's contract does not declare", v)}
		}
		if !SubjectSafe(s) {
			return SubjectProblem{fmt.Sprintf("the subject uses %q, whose sensitivity %s is not allowed in a subject line (only S0 and S1)", v, s.Sensitivity)}
		}
	}
	return nil
}

// CheckTemplateVariables verifies that a template's declared variables are a subset of
// the contract: a template cannot use a variable its intent does not govern.
func CheckTemplateVariables(schema []string, c map[string]VariableSpec) error {
	var bad []string
	for _, v := range schema {
		if _, ok := c[v]; !ok {
			bad = append(bad, fmt.Sprintf("%q is not in the intent's contract", v))
		}
	}
	if len(bad) > 0 {
		return VariableProblem{bad}
	}
	return nil
}

// CheckVariables validates supplied values against the contract: every required
// variable present, nothing undeclared, and every value of its declared type and
// length. It returns every problem at once.
func CheckVariables(c map[string]VariableSpec, vars map[string]string) error {
	var problems []string
	names := make([]string, 0, len(c))
	for n := range c {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		spec := c[n]
		v, supplied := vars[n]
		if !supplied || v == "" {
			if spec.Required {
				problems = append(problems, fmt.Sprintf("%s is required", n))
			}
			continue
		}
		if err := checkValue(spec, v); err != nil {
			problems = append(problems, fmt.Sprintf("%s %v", n, err))
		}
	}
	extra := []string{}
	for k := range vars {
		if _, ok := c[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		problems = append(problems, fmt.Sprintf("%s is not declared", k))
	}
	if len(problems) > 0 {
		return VariableProblem{problems}
	}
	return nil
}

func checkValue(s VariableSpec, v string) error {
	for _, r := range v {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fmt.Errorf("contains a control character")
		}
	}
	switch s.Type {
	case "STRING":
		limit := s.MaxLength
		if limit == 0 {
			limit = defaultVariableLength
		}
		if len([]rune(v)) > limit {
			return fmt.Errorf("is longer than %d characters", limit)
		}
	case "NUMBER":
		if !numberValueRe.MatchString(v) {
			return fmt.Errorf("must be a decimal number written as a string")
		}
	case "DATE":
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return fmt.Errorf("must be a date, YYYY-MM-DD")
		}
	case "ID":
		if !idValueRe.MatchString(v) {
			return fmt.Errorf("must be an identifier of letters, digits and _ . : -")
		}
	case "URL":
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(v) > maxVariableLength {
			return fmt.Errorf("must be an https URL without credentials")
		}
	}
	return nil
}

// ChannelAllowed reports whether the version permits a channel.
func (v IntentVersion) ChannelAllowed(channel string) bool { return inSet(v.AllowedChannels, channel) }
