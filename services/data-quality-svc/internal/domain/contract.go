package domain

// DQC-01 data contract and quality-rule registry (ZS-SVC-Z-001 section 5,
// docs/architecture/DQC_GAP_MATRIX.md step F).
//
// A contract is the technical statement of what data is expected of an asset and
// how its quality is measured. It is NOT a legal-applicability engine (that stays
// in PDC). A contract VERSION is immutable from the moment it is created: a change
// is a new version. What moves is only its lifecycle status, and only forward:
//
//	DRAFT -> REVIEW -> PUBLISHED -> DEPRECATED -> RETIRED     (DRAFT/REVIEW may also be RETIRED)
//
// Defaults taken where the specification leaves a decision open (section 19):
//   - OD-01 asset taxonomy: an asset is identified by a stable generated ID with a
//     tenant-unique key; its kind is a free lowercase token, not an enum.
//   - OD-06 tolerance authority: a tolerance is part of the contract, so it is
//     frozen with the version and changes only through a new, reviewed version.
//   - OD-07 rule DSL: semantic predicates are stored as opaque, versioned strings
//     naming the fields they read; this service validates their field references,
//     it does not execute them.
//   - Certification profile: when none is given, every ERROR and BLOCKER rule is
//     mandatory. The default is computed and STORED, so it cannot drift later.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	PrefixAsset    = "dca_"
	PrefixContract = "dct_"
)

type ContractStatus string

const (
	ContractDraft      ContractStatus = "DRAFT"
	ContractReview     ContractStatus = "REVIEW"
	ContractPublished  ContractStatus = "PUBLISHED"
	ContractDeprecated ContractStatus = "DEPRECATED"
	ContractRetired    ContractStatus = "RETIRED"
)

type Severity string

const (
	SeverityInfo    Severity = "INFO"
	SeverityWarning Severity = "WARNING"
	SeverityError   Severity = "ERROR"
	SeverityBlocker Severity = "BLOCKER"
)

type Compatibility string

const (
	CompatBackward Compatibility = "BACKWARD"
	CompatForward  Compatibility = "FORWARD"
	CompatFull     Compatibility = "FULL"
	CompatNone     Compatibility = "NONE"
)

var (
	validSeverity = map[Severity]bool{SeverityInfo: true, SeverityWarning: true, SeverityError: true, SeverityBlocker: true}
	validCompat   = map[Compatibility]bool{CompatBackward: true, CompatForward: true, CompatFull: true, CompatNone: true}
	validDim      = map[string]bool{
		"completeness": true, "validity": true, "consistency": true, "uniqueness": true,
		"accuracy": true, "timeliness": true, "integrity": true, "traceability": true,
	}
	validFieldType = map[string]bool{
		"string": true, "integer": true, "decimal": true, "boolean": true, "date": true, "timestamp": true, "json": true,
	}
	validRounding = map[string]bool{"HALF_UP": true, "HALF_EVEN": true, "DOWN": true}
	assetKeyRE    = regexp.MustCompile(`^[a-z][a-z0-9]*([._-][a-z0-9]+)*$`)
	kindRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	fieldNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,127}$`)
)

// severityRank orders severities for "weakened" detection.
var severityRank = map[Severity]int{SeverityInfo: 0, SeverityWarning: 1, SeverityError: 2, SeverityBlocker: 3}

type SchemaField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Format   string `json:"format,omitempty"`
}

// Tolerance is explicit: kind, magnitude, scale/rounding semantics and the
// population it is measured over. None of these has a default.
type Tolerance struct {
	Kind       string `json:"kind"`  // ABSOLUTE or PERCENT
	Value      string `json:"value"` // decimal text, >= 0 (PERCENT: <= 100)
	Scale      int    `json:"scale"`
	Rounding   string `json:"rounding"`
	Population string `json:"population"`
}

type SemanticPredicate struct {
	Name       string   `json:"name"`
	Expression string   `json:"expression"`
	Fields     []string `json:"fields"`
}

type ContractRule struct {
	RuleKey     string     `json:"rule_key"`
	Dimension   string     `json:"dimension"`
	Severity    Severity   `json:"severity"`
	Description string     `json:"description"`
	Predicate   string     `json:"predicate,omitempty"`
	Tolerance   *Tolerance `json:"tolerance,omitempty"`
}

type CertificationProfile struct {
	MandatoryRules []string `json:"mandatory_rules"`
	// AllowRiskAcceptance lets certification proceed past an unresolved BLOCKER
	// exception when a governed risk acceptance is recorded (section 8). False unless asked.
	AllowRiskAcceptance bool `json:"allow_risk_acceptance"`
	// ValidityWindowSeconds is how long a certification issued against this contract stays reliable. Zero means
	// it never goes stale. It is frozen with the version and copied onto each certification when it is issued
	// (ZS-SVC-Z-001 DQC-016 CERTIFICATION_STALE).
	ValidityWindowSeconds int64 `json:"validity_window_seconds,omitempty"`
}

// MaxValidityWindowSeconds bounds a validity window at ten years: a longer one is "never" written as a number.
const MaxValidityWindowSeconds = 10 * 365 * 24 * 3600

// ContractBody is everything a version freezes.
type ContractBody struct {
	Schema               []SchemaField        `json:"schema"`
	Predicates           []SemanticPredicate  `json:"predicates"`
	Rules                []ContractRule       `json:"rules"`
	Compatibility        Compatibility        `json:"compatibility"`
	CertificationProfile CertificationProfile `json:"certification_profile"`
	// PolicyRef optionally points at the PDC decision or domain policy version the
	// technical requirements depend on. It is a reference, never an evaluation.
	PolicyRef string `json:"policy_ref,omitempty"`
}

type ContractVersion struct {
	ContractID    string          `json:"contract_id"`
	TenantID      string          `json:"tenant_id"`
	AssetID       string          `json:"asset_id"`
	AssetKey      string          `json:"asset_key"`
	AssetKind     string          `json:"asset_kind"`
	VersionNumber int             `json:"version_number"`
	Status        ContractStatus  `json:"status"`
	Body          ContractBody    `json:"body"`
	ContentSHA256 string          `json:"content_sha256"`
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	SubmittedBy   *string         `json:"submitted_by,omitempty"`
	SubmittedAt   *time.Time      `json:"submitted_at,omitempty"`
	PublishedBy   *string         `json:"published_by,omitempty"`
	PublishedAt   *time.Time      `json:"published_at,omitempty"`
	DeprecatedAt  *time.Time      `json:"deprecated_at,omitempty"`
	RetiredAt     *time.Time      `json:"retired_at,omitempty"`
	LifecycleNote *string         `json:"lifecycle_note,omitempty"`
	Compatibility json.RawMessage `json:"compatibility_evidence,omitempty"`
	ComparedToVer *int            `json:"compared_to_version,omitempty"`
}

type CreateContractRequest struct {
	AssetKey  string       `json:"asset_key"`
	AssetKind string       `json:"asset_kind"`
	Body      ContractBody `json:"body"`
}

type PublishContractRequest struct {
	// AcknowledgeBreaking is required to publish a breaking change under compatibility NONE.
	AcknowledgeBreaking bool `json:"acknowledge_breaking"`
}

type CompatibilityCheckRequest struct {
	// AgainstVersion is REQUIRED. A comparison never resolves "the latest".
	AgainstVersion int          `json:"against_version"`
	Candidate      ContractBody `json:"candidate"`
}

type BreakingChange struct {
	Field  string `json:"field"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type SeverityChange struct {
	RuleKey string   `json:"rule_key"`
	From    Severity `json:"from"`
	To      Severity `json:"to"`
	Kind    string   `json:"kind"` // WEAKENED, STRENGTHENED, RULE_REMOVED
}

// CompatibilityReport is the explicit breaking-change evidence DQC-01 requires.
type CompatibilityReport struct {
	Mode            Compatibility    `json:"mode"`
	AgainstVersion  int              `json:"against_version"`
	Compatible      bool             `json:"compatible"`
	Breaking        []BreakingChange `json:"breaking"`
	SeverityChanges []SeverityChange `json:"severity_changes"`
}

const (
	ErrKindFieldRemoved       = "REQUIRED_FIELD_REMOVED"
	ErrKindTypeChanged        = "TYPE_CHANGED"
	ErrKindFormatChanged      = "FORMAT_CHANGED"
	ErrKindRequiredFieldAdded = "REQUIRED_FIELD_ADDED"
	ErrKindNullTightened      = "NULLABLE_TIGHTENED"
	ErrKindNullLoosened       = "NULLABLE_LOOSENED"
)

var (
	ErrContractNotFound         = errorString("DQC-001 contract not found")
	ErrContractVersionUnknown   = errorString("DQC-002 contract version unknown")
	ErrSchemaIncompatible       = errorString("DQC-003 the change is incompatible with the declared compatibility mode")
	ErrContractInvalid          = errorString("contract is invalid")
	ErrContractTransition       = errorString("that lifecycle transition is not allowed")
	ErrContractDraftOpen        = errorString("the asset already has an open draft or review version; publish or retire it first")
	ErrContractAssetKindClash   = errorString("the asset already exists with a different kind")
	ErrContractNotIndependent   = errorString("the publisher must be someone other than the author and the submitter")
	ErrBreakingNotAcknowledged  = errorString("a breaking change under compatibility NONE requires acknowledge_breaking")
	ErrContractNotPublishedBase = errorString("the version compared against must be PUBLISHED or DEPRECATED")
)

// Normalize puts a body in canonical form (sorted, defaults made explicit) so the
// same contract always hashes the same. It is applied before validation.
func (b *ContractBody) Normalize() {
	if b.Schema == nil {
		b.Schema = []SchemaField{}
	}
	if b.Predicates == nil {
		b.Predicates = []SemanticPredicate{}
	}
	if b.Rules == nil {
		b.Rules = []ContractRule{}
	}
	sort.Slice(b.Schema, func(i, j int) bool { return b.Schema[i].Name < b.Schema[j].Name })
	sort.Slice(b.Predicates, func(i, j int) bool { return b.Predicates[i].Name < b.Predicates[j].Name })
	for i := range b.Predicates {
		if b.Predicates[i].Fields == nil {
			b.Predicates[i].Fields = []string{}
		}
		sort.Strings(b.Predicates[i].Fields)
	}
	sort.Slice(b.Rules, func(i, j int) bool { return b.Rules[i].RuleKey < b.Rules[j].RuleKey })
	if len(b.CertificationProfile.MandatoryRules) == 0 {
		// The stored default: every ERROR and BLOCKER rule.
		m := []string{}
		for _, r := range b.Rules {
			if r.Severity == SeverityError || r.Severity == SeverityBlocker {
				m = append(m, r.RuleKey)
			}
		}
		b.CertificationProfile.MandatoryRules = m
	}
	sort.Strings(b.CertificationProfile.MandatoryRules)
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrContractInvalid, fmt.Sprintf(format, a...))
}

// ValidateAssetIdentity checks the stable asset identity fields.
func ValidateAssetIdentity(key, kind string) error {
	if !assetKeyRE.MatchString(key) || len(key) > 128 {
		return invalid("asset_key must be lowercase dotted/dashed tokens, got %q", key)
	}
	if !kindRE.MatchString(kind) {
		return invalid("asset_kind must be a lowercase token, got %q", kind)
	}
	return nil
}

// Validate checks a NORMALIZED body. It never guesses: an ambiguous or incomplete
// contract is refused (INV-15).
func (b ContractBody) Validate() error {
	if len(b.Schema) == 0 {
		return invalid("schema must declare at least one field")
	}
	fields := map[string]bool{}
	for _, f := range b.Schema {
		if !fieldNameRE.MatchString(f.Name) {
			return invalid("field name %q is not valid", f.Name)
		}
		if fields[f.Name] {
			return invalid("field %q is declared twice", f.Name)
		}
		fields[f.Name] = true
		if !validFieldType[f.Type] {
			return invalid("field %q has unknown type %q", f.Name, f.Type)
		}
	}
	if !validCompat[b.Compatibility] {
		return invalid("compatibility must be one of BACKWARD, FORWARD, FULL, NONE (got %q)", b.Compatibility)
	}
	preds := map[string]bool{}
	for _, p := range b.Predicates {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Expression) == "" {
			return invalid("a predicate needs a name and an expression")
		}
		if preds[p.Name] {
			return invalid("predicate %q is declared twice", p.Name)
		}
		preds[p.Name] = true
		if len(p.Fields) == 0 {
			return invalid("predicate %q must name the fields it reads", p.Name)
		}
		for _, f := range p.Fields {
			if !fields[f] {
				return invalid("predicate %q reads field %q which is not in the schema", p.Name, f)
			}
		}
	}
	if len(b.Rules) == 0 {
		return invalid("a contract needs at least one rule")
	}
	rules := map[string]ContractRule{}
	for _, r := range b.Rules {
		if strings.TrimSpace(r.RuleKey) == "" {
			return invalid("a rule needs a rule_key")
		}
		if _, dup := rules[r.RuleKey]; dup {
			return invalid("rule %q is declared twice", r.RuleKey)
		}
		rules[r.RuleKey] = r
		if !validDim[r.Dimension] {
			return invalid("rule %q has unknown dimension %q", r.RuleKey, r.Dimension)
		}
		if !validSeverity[r.Severity] {
			return invalid("rule %q has unknown severity %q (INFO, WARNING, ERROR, BLOCKER)", r.RuleKey, r.Severity)
		}
		if r.Predicate != "" && !preds[r.Predicate] {
			return invalid("rule %q uses predicate %q which is not declared", r.RuleKey, r.Predicate)
		}
		if r.Tolerance != nil {
			if err := r.Tolerance.validate(r.RuleKey); err != nil {
				return err
			}
		}
	}
	if w := b.CertificationProfile.ValidityWindowSeconds; w < 0 || w > MaxValidityWindowSeconds {
		return invalid("validity_window_seconds must be between 0 (never stale) and %d", MaxValidityWindowSeconds)
	}
	seen := map[string]bool{}
	for _, k := range b.CertificationProfile.MandatoryRules {
		if _, ok := rules[k]; !ok {
			return invalid("certification profile names rule %q which the contract does not define", k)
		}
		seen[k] = true
	}
	// A BLOCKER cannot be left out of what certification requires.
	for _, r := range b.Rules {
		if r.Severity == SeverityBlocker && !seen[r.RuleKey] {
			return invalid("BLOCKER rule %q must be mandatory for certification", r.RuleKey)
		}
	}
	return nil
}

func (t Tolerance) validate(rule string) error {
	if t.Kind != "ABSOLUTE" && t.Kind != "PERCENT" {
		return invalid("rule %q tolerance kind must be ABSOLUTE or PERCENT", rule)
	}
	v, ok := new(big.Rat).SetString(t.Value)
	if !ok || v.Sign() < 0 {
		return invalid("rule %q tolerance value must be a non-negative decimal", rule)
	}
	if t.Kind == "PERCENT" && v.Cmp(big.NewRat(100, 1)) > 0 {
		return invalid("rule %q percent tolerance cannot exceed 100", rule)
	}
	if t.Scale < 0 || t.Scale > 18 {
		return invalid("rule %q tolerance scale must be 0..18", rule)
	}
	if !validRounding[t.Rounding] {
		return invalid("rule %q tolerance needs a rounding mode (HALF_UP, HALF_EVEN, DOWN)", rule)
	}
	if strings.TrimSpace(t.Population) == "" {
		return invalid("rule %q tolerance must define its population", rule)
	}
	return nil
}

// ContentHash is the SHA-256 of the canonical JSON of a normalized body.
func (b ContractBody) ContentHash() string {
	raw, _ := json.Marshal(b) // struct field order is fixed; slices were sorted by Normalize
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CheckCompatibility compares candidate against an earlier body under the
// CANDIDATE's declared mode and reports every breaking change. It is pure and
// deterministic: the same inputs always give the same evidence.
//
// BACKWARD: the candidate can read data written to the old schema.
// FORWARD:  the old schema can read data written to the candidate.
// FULL:     both. NONE: no guarantee is claimed, but the evidence is still produced.
//
// Removing a non-nullable field is breaking in every mode except NONE: whoever
// depends on it loses it (NP-03). Rules are compared for severity only; weakening a
// rule is reported, because severity cannot be edited retroactively (section 5).
func CheckCompatibility(old, cand ContractBody, againstVersion int) CompatibilityReport {
	mode := cand.Compatibility
	rep := CompatibilityReport{Mode: mode, AgainstVersion: againstVersion, Breaking: []BreakingChange{}, SeverityChanges: []SeverityChange{}}
	backward := mode == CompatBackward || mode == CompatFull
	forward := mode == CompatForward || mode == CompatFull
	enforced := mode != CompatNone

	oldF := map[string]SchemaField{}
	for _, f := range old.Schema {
		oldF[f.Name] = f
	}
	newF := map[string]SchemaField{}
	for _, f := range cand.Schema {
		newF[f.Name] = f
	}
	add := func(field, kind, detail string) {
		rep.Breaking = append(rep.Breaking, BreakingChange{Field: field, Kind: kind, Detail: detail})
	}
	names := make([]string, 0, len(oldF)+len(newF))
	for n := range oldF {
		names = append(names, n)
	}
	for n := range newF {
		if _, ok := oldF[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		o, inOld := oldF[n]
		c, inNew := newF[n]
		switch {
		case inOld && !inNew:
			if !o.Nullable {
				add(n, ErrKindFieldRemoved, "a non-nullable field was removed")
			}
		case !inOld && inNew:
			if !c.Nullable && backward {
				add(n, ErrKindRequiredFieldAdded, "a non-nullable field was added; old data cannot supply it")
			}
		default:
			if o.Type != c.Type {
				add(n, ErrKindTypeChanged, fmt.Sprintf("type changed from %s to %s", o.Type, c.Type))
			}
			if o.Format != c.Format {
				add(n, ErrKindFormatChanged, fmt.Sprintf("format changed from %q to %q", o.Format, c.Format))
			}
			if o.Nullable && !c.Nullable && backward {
				add(n, ErrKindNullTightened, "a nullable field became non-nullable; old data may contain nulls")
			}
			if !o.Nullable && c.Nullable && forward {
				add(n, ErrKindNullLoosened, "a non-nullable field became nullable; old readers expect a value")
			}
		}
	}

	oldR := map[string]ContractRule{}
	for _, r := range old.Rules {
		oldR[r.RuleKey] = r
	}
	newR := map[string]ContractRule{}
	for _, r := range cand.Rules {
		newR[r.RuleKey] = r
	}
	keys := make([]string, 0, len(oldR))
	for k := range oldR {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := oldR[k]
		c, ok := newR[k]
		switch {
		case !ok:
			rep.SeverityChanges = append(rep.SeverityChanges, SeverityChange{RuleKey: k, From: o.Severity, Kind: "RULE_REMOVED"})
		case severityRank[c.Severity] < severityRank[o.Severity]:
			rep.SeverityChanges = append(rep.SeverityChanges, SeverityChange{RuleKey: k, From: o.Severity, To: c.Severity, Kind: "WEAKENED"})
		case severityRank[c.Severity] > severityRank[o.Severity]:
			rep.SeverityChanges = append(rep.SeverityChanges, SeverityChange{RuleKey: k, From: o.Severity, To: c.Severity, Kind: "STRENGTHENED"})
		}
	}
	rep.Compatible = !enforced || len(rep.Breaking) == 0
	return rep
}

// Breaking reports whether the report found breaking changes, whether or not
// the declared mode enforces them.
func (r CompatibilityReport) IsBreaking() bool { return len(r.Breaking) > 0 }
