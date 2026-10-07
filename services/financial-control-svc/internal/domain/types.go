// Package domain holds financial-control-svc's canonical control model
// (ZS-CONTROL-001 §6). This service verifies financial truth; it never becomes
// a second ledger (Invariant 6): nothing here posts or edits accounting state.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type errorString string

func (e errorString) Error() string { return string(e) }

const (
	ErrNotFound          = errorString("not found")
	ErrInvalidTransition = errorString("invalid state transition")
	ErrInvalidArgument   = errorString("invalid argument")
	ErrConflict          = errorString("version conflict")
	ErrDuplicate         = errorString("already exists")
	ErrSelfApproval      = errorString("the creator of a control artifact cannot approve it")
	ErrToleranceWidened  = errorString("tolerance cannot be changed on an existing run; create a new policy version and a new run")

	ErrAuthorizationDenied             = errorString("authorization denied for this financial control action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	ErrIdentityMissing     = errorString("caller identity missing")
	ErrTenantScopeMissing  = errorString("caller tenant scope missing")
	ErrTenantScopeMismatch = errorString("tenant_id does not match the caller's verified tenant scope")
	ErrInvalidIdentifier   = errorString("malformed identifier or date")
)

type ControlType string
type RiskTier string
type Frequency string
type TriggerType string

var (
	controlTypes = map[string]bool{"BALANCE": true, "TRANSACTION": true, "INTERFACE": true, "REPORTING": true,
		"EXTERNAL": true, "PREVENTIVE": true, "MONITORING": true, "REVIEW": true, "ENTITY_LEVEL": true}
	riskTiers   = map[string]bool{"KEY": true, "STANDARD": true, "LOW": true}
	frequencies = map[string]bool{"EVENT_DRIVEN": true, "INTRADAY": true, "DAILY": true, "PERIOD_END": true,
		"ON_DEMAND": true, "PER_BATCH": true, "CONTINUOUS": true}
	triggers = map[string]bool{"EVENT": true, "INTRADAY": true, "DAILY": true, "PERIOD_END": true, "ON_DEMAND": true}
	// §5 financial assertions.
	assertions = map[string]bool{"COMPLETENESS": true, "ACCURACY": true, "EXISTENCE": true, "VALUATION": true,
		"RIGHTS_OBLIGATIONS": true, "CUTOFF": true, "CLASSIFICATION": true, "PRESENTATION": true,
		"AUTHORIZATION": true, "OCCURRENCE": true, "REPORTING_RELIABILITY": true}
	controlCodeRe = regexp.MustCompile(`^[A-Z]{2,8}-CTRL-[0-9]{3,4}$`)
	currencyRe    = regexp.MustCompile(`^[A-Z]{3}$`)
	digestRe      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ─── ControlDefinition ───────────────────────────────────────────────────────

type ControlDefinition struct {
	ControlDefinitionID string          `json:"control_definition_id"`
	TenantID            string          `json:"tenant_id"`
	ControlCode         string          `json:"control_code"`
	Name                string          `json:"name"`
	Domain              string          `json:"domain"`
	ControlType         string          `json:"control_type"`
	Assertions          []string        `json:"assertions"`
	RiskTier            string          `json:"risk_tier"`
	Frequency           string          `json:"frequency"`
	CloseGating         bool            `json:"close_gating"`
	Scope               json.RawMessage `json:"scope"`
	OwnerRole           string          `json:"owner_role"`
	ReviewerRole        string          `json:"reviewer_role"`
	CertifierRole       string          `json:"certifier_role"`
	SourceSpec          json.RawMessage `json:"source_spec"`
	TargetSpec          json.RawMessage `json:"target_spec"`
	EvidencePolicy      json.RawMessage `json:"evidence_policy"`
	PolicyRefs          []string        `json:"policy_refs"`
	CreatedBy           string          `json:"created_by"`
	CreatedAt           time.Time       `json:"created_at"`
	CorrelationID       string          `json:"correlation_id"`
	// LatestRuleVersion is populated on reads.
	LatestRuleVersion *int `json:"latest_rule_version,omitempty"`
}

type CreateControlDefinitionRequest struct {
	ControlCode    string          `json:"control_code"`
	Name           string          `json:"name"`
	Domain         string          `json:"domain"`
	ControlType    string          `json:"control_type"`
	Assertions     []string        `json:"assertions"`
	RiskTier       string          `json:"risk_tier"`
	Frequency      string          `json:"frequency"`
	CloseGating    bool            `json:"close_gating"`
	Scope          json.RawMessage `json:"scope"`
	OwnerRole      string          `json:"owner_role"`
	ReviewerRole   string          `json:"reviewer_role"`
	CertifierRole  string          `json:"certifier_role"`
	SourceSpec     json.RawMessage `json:"source_spec"`
	TargetSpec     json.RawMessage `json:"target_spec"`
	EvidencePolicy json.RawMessage `json:"evidence_policy"`
	PolicyRefs     []string        `json:"policy_refs"`
	// Initial rule version (v1) is created with the definition.
	InitialLogic         json.RawMessage `json:"initial_logic"`
	InitialTestPack      string          `json:"initial_test_pack_version"`
	InitialEffectiveFrom string          `json:"initial_effective_from"`
}

// Validate enforces Invariant 1: every control has a stable id, a named owner
// role, a defined assertion, scope, frequency/trigger, source population,
// target/expected condition and an evidence requirement.
func (r *CreateControlDefinitionRequest) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, msg) }
	r.ControlCode = strings.ToUpper(strings.TrimSpace(r.ControlCode))
	if !controlCodeRe.MatchString(r.ControlCode) {
		return bad("control_code must look like FIN-CTRL-001")
	}
	if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Domain) == "" {
		return bad("name and domain are required")
	}
	if !controlTypes[r.ControlType] {
		return bad("control_type is not a recognised control level")
	}
	if !riskTiers[r.RiskTier] {
		return bad("risk_tier must be KEY, STANDARD or LOW")
	}
	if !frequencies[r.Frequency] {
		return bad("frequency is not a recognised trigger class")
	}
	if len(r.Assertions) == 0 {
		return bad("at least one financial assertion is required")
	}
	for i, a := range r.Assertions {
		a = strings.ToUpper(strings.TrimSpace(a))
		if !assertions[a] {
			return bad(fmt.Sprintf("unknown assertion %q", a))
		}
		r.Assertions[i] = a
	}
	if strings.TrimSpace(r.OwnerRole) == "" {
		return bad("owner_role is required (Invariant 1)")
	}
	if r.RiskTier == "KEY" && (strings.TrimSpace(r.ReviewerRole) == "" || strings.TrimSpace(r.CertifierRole) == "") {
		return bad("key controls require reviewer_role and certifier_role")
	}
	if r.CloseGating && r.RiskTier != "KEY" {
		return bad("only key controls may be close-gating")
	}
	for name, raw := range map[string]json.RawMessage{"scope": r.Scope, "source_spec": r.SourceSpec,
		"target_spec": r.TargetSpec, "evidence_policy": r.EvidencePolicy, "initial_logic": r.InitialLogic} {
		if len(raw) > 0 && !json.Valid(raw) {
			return bad(name + " is not valid JSON")
		}
	}
	if len(r.InitialLogic) == 0 {
		return bad("initial_logic is required")
	}
	logic, err := ParseRuleLogic(r.InitialLogic) // strict: unknown keys are errors
	if err != nil {
		return err
	}
	// Preventive/monitoring controls test an expected condition, not a second population.
	if len(r.SourceSpec) == 0 || string(r.SourceSpec) == "{}" || string(r.SourceSpec) == "null" {
		return bad("source_spec (source population definition) is required")
	}
	if logic.TwoSided() && (len(r.TargetSpec) == 0 || string(r.TargetSpec) == "{}" || string(r.TargetSpec) == "null") &&
		r.ControlType != "PREVENTIVE" && r.ControlType != "MONITORING" {
		return bad("target_spec (target population or expected condition) is required")
	}
	if len(r.EvidencePolicy) == 0 || string(r.EvidencePolicy) == "{}" || string(r.EvidencePolicy) == "null" {
		return bad("evidence_policy is required (Invariant 1)")
	}
	if len(r.InitialLogic) == 0 {
		return bad("initial_logic is required")
	}
	if _, err := time.Parse("2006-01-02", r.InitialEffectiveFrom); err != nil {
		return bad("initial_effective_from must be YYYY-MM-DD")
	}
	return nil
}

// ─── Rule versions ───────────────────────────────────────────────────────────

type ControlRuleVersion struct {
	RuleVersionID       string          `json:"rule_version_id"`
	TenantID            string          `json:"tenant_id"`
	ControlDefinitionID string          `json:"control_definition_id"`
	RuleVersion         int             `json:"rule_version"`
	LogicDigest         string          `json:"logic_digest"`
	Logic               json.RawMessage `json:"logic"`
	TestPackVersion     string          `json:"test_pack_version"`
	EffectiveFrom       string          `json:"effective_from"`
	EffectiveTo         *string         `json:"effective_to,omitempty"`
	ApprovedBy          *string         `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time      `json:"approved_at,omitempty"`
	CreatedBy           string          `json:"created_by"`
	CreatedAt           time.Time       `json:"created_at"`
}

type CreateRuleVersionRequest struct {
	Logic           json.RawMessage `json:"logic"`
	TestPackVersion string          `json:"test_pack_version"`
	EffectiveFrom   string          `json:"effective_from"`
}

func (r *CreateRuleVersionRequest) Validate() error {
	if len(r.Logic) == 0 || !json.Valid(r.Logic) {
		return fmt.Errorf("%w: logic must be valid JSON", ErrInvalidArgument)
	}
	if _, err := ParseRuleLogic(r.Logic); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", r.EffectiveFrom); err != nil {
		return fmt.Errorf("%w: effective_from must be YYYY-MM-DD", ErrInvalidArgument)
	}
	return nil
}

// DigestLogic returns the canonical sha256 digest of a rule/config document.
// The document is re-encoded through a generic value so map keys are emitted in
// sorted order: two semantically identical documents hash identically
// (Invariants 3, 4, 16).
func DigestLogic(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("%w: logic: %v", ErrInvalidArgument, err)
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func ValidDigest(s string) bool { return digestRe.MatchString(s) }

// ─── Tolerance / materiality ─────────────────────────────────────────────────

// TolerancePolicy answers "how much difference is operationally acceptable for
// this comparison". Amounts are decimal STRINGS — never float64 (ZS-DATA-001 D06).
type TolerancePolicy struct {
	ToleranceID         string    `json:"tolerance_id"`
	TenantID            string    `json:"tenant_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	Metric              string    `json:"metric"`
	ToleranceVersion    int       `json:"tolerance_version"`
	AbsoluteTolerance   string    `json:"absolute_tolerance"`
	PercentageTolerance string    `json:"percentage_tolerance"`
	PercentageBase      string    `json:"percentage_base"`
	DateToleranceDays   int       `json:"date_tolerance_days"`
	DateBasis           string    `json:"date_basis"`
	Currency            string    `json:"currency"`
	PermittedContexts   []string  `json:"permitted_contexts"`
	Rationale           string    `json:"rationale"`
	EffectiveFrom       string    `json:"effective_from"`
	ApprovedBy          string    `json:"approved_by"`
	CreatedBy           string    `json:"created_by"`
	CreatedAt           time.Time `json:"created_at"`
}

type CreateTolerancePolicyRequest struct {
	LegalEntityID       string   `json:"legal_entity_id"`
	Metric              string   `json:"metric"`
	AbsoluteTolerance   string   `json:"absolute_tolerance"`
	PercentageTolerance string   `json:"percentage_tolerance"`
	PercentageBase      string   `json:"percentage_base"`
	DateToleranceDays   int      `json:"date_tolerance_days"`
	DateBasis           string   `json:"date_basis"`
	Currency            string   `json:"currency"`
	PermittedContexts   []string `json:"permitted_contexts"`
	Rationale           string   `json:"rationale"`
	EffectiveFrom       string   `json:"effective_from"`
	// ApprovedBy is the independent approver; it must differ from the caller.
	ApprovedBy string `json:"approved_by"`
}

func (r *CreateTolerancePolicyRequest) Validate(actor string) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, msg) }
	if strings.TrimSpace(r.Metric) == "" {
		return bad("metric is required")
	}
	if !currencyRe.MatchString(r.Currency) {
		return bad("currency must be a 3-letter ISO 4217 code")
	}
	if strings.TrimSpace(r.Rationale) == "" {
		return bad("rationale is required")
	}
	abs, err := parseNonNegDecimal(r.AbsoluteTolerance)
	if err != nil {
		return bad("absolute_tolerance: " + err.Error())
	}
	pct, err := parseNonNegDecimal(r.PercentageTolerance)
	if err != nil {
		return bad("percentage_tolerance: " + err.Error())
	}
	if pct.Sign() > 0 && strings.TrimSpace(r.PercentageBase) == "" {
		return bad("percentage tolerance requires an explicit percentage_base (§11)")
	}
	if r.DateToleranceDays < 0 {
		return bad("date_tolerance_days must be >= 0")
	}
	if r.DateBasis == "" {
		r.DateBasis = "CALENDAR"
	}
	if r.DateBasis != "CALENDAR" && r.DateBasis != "BUSINESS" {
		return bad("date_basis must be CALENDAR or BUSINESS")
	}
	_ = abs // an all-zero policy is legal: it means "exact match required".
	if _, err := time.Parse("2006-01-02", r.EffectiveFrom); err != nil {
		return bad("effective_from must be YYYY-MM-DD")
	}
	if strings.TrimSpace(r.ApprovedBy) == "" {
		return bad("approved_by is required: tolerance changes are independently approved (§27)")
	}
	if r.ApprovedBy == actor {
		return ErrSelfApproval
	}
	return nil
}

type MaterialityPolicy struct {
	MaterialityID       string    `json:"materiality_id"`
	TenantID            string    `json:"tenant_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	ReportingBasis      string    `json:"reporting_basis"`
	MaterialityVersion  int       `json:"materiality_version"`
	AmountThreshold     string    `json:"amount_threshold"`
	AggregateThreshold  string    `json:"aggregate_threshold"`
	Currency            string    `json:"currency"`
	QualitativeTriggers []string  `json:"qualitative_triggers"`
	AggregationBasis    string    `json:"aggregation_basis"`
	EffectiveFrom       string    `json:"effective_from"`
	ApprovedBy          string    `json:"approved_by"`
	CreatedBy           string    `json:"created_by"`
	CreatedAt           time.Time `json:"created_at"`
}

type CreateMaterialityPolicyRequest struct {
	LegalEntityID       string   `json:"legal_entity_id"`
	ReportingBasis      string   `json:"reporting_basis"`
	AmountThreshold     string   `json:"amount_threshold"`
	AggregateThreshold  string   `json:"aggregate_threshold"`
	Currency            string   `json:"currency"`
	QualitativeTriggers []string `json:"qualitative_triggers"`
	AggregationBasis    string   `json:"aggregation_basis"`
	EffectiveFrom       string   `json:"effective_from"`
	ApprovedBy          string   `json:"approved_by"`
}

func (r *CreateMaterialityPolicyRequest) Validate(actor string) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, msg) }
	if strings.TrimSpace(r.ReportingBasis) == "" {
		return bad("reporting_basis is required")
	}
	if !currencyRe.MatchString(r.Currency) {
		return bad("currency must be a 3-letter ISO 4217 code")
	}
	amt, err := parseNonNegDecimal(r.AmountThreshold)
	if err != nil {
		return bad("amount_threshold: " + err.Error())
	}
	agg, err := parseNonNegDecimal(r.AggregateThreshold)
	if err != nil {
		return bad("aggregate_threshold: " + err.Error())
	}
	// §11 aggregation: repeated small items may be material by pattern, so the
	// aggregate threshold can never be looser than the individual one.
	if agg.Cmp(amt) < 0 {
		return bad("aggregate_threshold cannot be smaller than amount_threshold")
	}
	if r.AggregationBasis == "" {
		r.AggregationBasis = "ENTITY_PERIOD"
	}
	if _, err := time.Parse("2006-01-02", r.EffectiveFrom); err != nil {
		return bad("effective_from must be YYYY-MM-DD")
	}
	if strings.TrimSpace(r.ApprovedBy) == "" {
		return bad("approved_by is required")
	}
	if r.ApprovedBy == actor {
		return ErrSelfApproval
	}
	return nil
}

// ─── ControlRun ──────────────────────────────────────────────────────────────

type ControlRun struct {
	RunID               string             `json:"run_id"`
	TenantID            string             `json:"tenant_id"`
	LegalEntityID       string             `json:"legal_entity_id"`
	ControlDefinitionID string             `json:"control_definition_id"`
	RuleVersion         int                `json:"rule_version"`
	RuleDigest          string             `json:"rule_digest"`
	ToleranceID         *string            `json:"tolerance_id,omitempty"`
	MaterialityID       *string            `json:"materiality_id,omitempty"`
	PeriodID            string             `json:"period_id"`
	Scope               json.RawMessage    `json:"scope"`
	TriggerType         string             `json:"trigger_type"`
	TriggerReason       string             `json:"trigger_reason"`
	PriorRunID          *string            `json:"prior_run_id,omitempty"`
	SupersededByRunID   *string            `json:"superseded_by_run_id,omitempty"`
	LifecycleState      LifecycleState     `json:"lifecycle_state"`
	ResultState         ResultState        `json:"result_state"`
	CertificationState  CertificationState `json:"certification_state"`
	Version             int                `json:"version"`
	StartedAt           *time.Time         `json:"started_at,omitempty"`
	CompletedAt         *time.Time         `json:"completed_at,omitempty"`
	CreatedBy           string             `json:"created_by"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	CorrelationID       string             `json:"correlation_id"`
	// Derived, non-authoritative attention signals (§7): never stored, never
	// overwrite an authoritative state.
	Attention []string `json:"attention,omitempty"`
}

type CreateRunRequest struct {
	ControlDefinitionID string          `json:"control_definition_id"`
	LegalEntityID       string          `json:"legal_entity_id"`
	PeriodID            string          `json:"period_id"`
	Scope               json.RawMessage `json:"scope"`
	TriggerType         string          `json:"trigger_type"`
	// Reason is mandatory for ON_DEMAND runs and for any run naming a prior run (§8, §25).
	Reason     string `json:"reason"`
	PriorRunID string `json:"prior_run_id"`
	// Tolerance/materiality are RESOLVED by the server at creation and pinned.
	// The caller may name a specific approved version but can never supply values.
	ToleranceID   string `json:"tolerance_id"`
	MaterialityID string `json:"materiality_id"`
}

func (r *CreateRunRequest) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, msg) }
	if r.ControlDefinitionID == "" || r.LegalEntityID == "" {
		return bad("control_definition_id and legal_entity_id are required")
	}
	if !triggers[r.TriggerType] {
		return bad("trigger_type must be one of EVENT, INTRADAY, DAILY, PERIOD_END, ON_DEMAND")
	}
	if len(r.Scope) > 0 && !json.Valid(r.Scope) {
		return bad("scope is not valid JSON")
	}
	if (r.TriggerType == "ON_DEMAND" || r.PriorRunID != "") && strings.TrimSpace(r.Reason) == "" {
		return bad("reason is required for on-demand runs and runs related to a prior run")
	}
	if r.TriggerType == "PERIOD_END" && strings.TrimSpace(r.PeriodID) == "" {
		return bad("period_id is required for period-end runs")
	}
	return nil
}

type Transition struct {
	TransitionID  int64     `json:"transition_id"`
	RunID         string    `json:"run_id"`
	Dimension     string    `json:"dimension"`
	FromState     string    `json:"from_state"`
	ToState       string    `json:"to_state"`
	Reason        string    `json:"reason"`
	ActorID       string    `json:"actor_id"`
	CorrelationID string    `json:"correlation_id"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type ListRunsFilter struct {
	ControlDefinitionID string
	LegalEntityID       string
	PeriodID            string
	LifecycleState      string
	Limit               int
	AfterCreatedAt      *time.Time
	AfterRunID          string
}
