package domain

import (
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI-04 governed advisory layer (/v1/anomaly-governance)
//
// Spec: ZoikoSuite Data/Search/Analytics/AI Service Specifications, AI-04
// "Anomaly / Exception Detection".
//
//   Owns:      AnomalyModel, DetectionRun, AnomalySignal, ReviewDisposition
//   Commands:  RunDetection, AcknowledgeSignal, EscalateForReview, CloseSignal
//   Lifecycle: Detected → ReviewPending → Confirmed / Dismissed / Escalated
//   Events:    AI.AnomalyDetected, AI.AnomalyDispositioned
//
// Explicitly NOT owned: autonomous adverse action, fraud accusation,
// write-off, blocking payment. Nothing here writes outside this service's own
// tables, and any attempt to attach such an action is refused
// (CheckAdvisoryBoundary).
//
// RunDetection accepts already-scored signal candidates (score, severity,
// feature evidence) computed by the caller's model; this layer governs the
// model registry, drift gating, review workflow and evidence trail around
// them.
// ─────────────────────────────────────────────────────────────────────────────

// Event names (Enterprise Domain Event Catalogue / AI-04 spec).
const (
	EventAnomalyDetected      = "AI.AnomalyDetected"
	EventAnomalyDispositioned = "AI.AnomalyDispositioned"
)

// ── AnomalyModel ────────────────────────────────────────────────────────────

// ModelStatus is whether a registered model may be used for detection.
type ModelStatus string

const (
	ModelActive   ModelStatus = "Active"
	ModelDisabled ModelStatus = "Disabled"
)

func (s ModelStatus) IsValid() bool { return s == ModelActive || s == ModelDisabled }

// DriftPolicy is what RunDetection does when the caller-observed drift is
// above the model's max_drift_bp (spec N-37: "escalate/degrade/disable per
// model policy").
type DriftPolicy string

const (
	// DriftPolicyBlock refuses the run (409 drift_threshold_exceeded);
	// nothing is created.
	DriftPolicyBlock DriftPolicy = "BLOCK"
	// DriftPolicyEscalate accepts the run but marks it drift_breached and
	// forces every signal to ReviewPending regardless of score: the output
	// is "review required", never trusted on its own.
	DriftPolicyEscalate DriftPolicy = "ESCALATE"
	// DriftPolicyDisable refuses the run and flips the model to Disabled so
	// later runs are refused until a publisher re-enables it.
	DriftPolicyDisable DriftPolicy = "DISABLE"
)

func (p DriftPolicy) IsValid() bool {
	return p == DriftPolicyBlock || p == DriftPolicyEscalate || p == DriftPolicyDisable
}

// AnomalyModel is the governed baseline/model registration for one
// (domain, model provider, model version). Version is the optimistic
// concurrency counter exposed as the ETag; it is unrelated to ModelVersion,
// which is the ML model's own release label.
type AnomalyModel struct {
	ModelID           string      `json:"model_id"`
	TenantID          string      `json:"tenant_id"`
	DomainName        string      `json:"domain_name"`
	ModelProvider     string      `json:"model_provider"`
	ModelVersion      string      `json:"model_version"`
	ReviewThresholdBP int32       `json:"review_threshold_bp"`
	MaxDriftBP        int32       `json:"max_drift_bp"`
	DriftPolicy       DriftPolicy `json:"drift_policy"`
	Status            ModelStatus `json:"status"`
	Version           int         `json:"version"`
	CreatedAt         time.Time   `json:"created_at"`
	CreatedBy         string      `json:"created_by"`
	UpdatedAt         time.Time   `json:"updated_at"`
	UpdatedBy         string      `json:"updated_by"`
}

// RegisterAnomalyModelRequest is the body of POST /v1/anomaly-governance/models.
type RegisterAnomalyModelRequest struct {
	BodyScope
	DomainName        string      `json:"domain_name"`
	ModelProvider     string      `json:"model_provider"`
	ModelVersion      string      `json:"model_version"`
	ReviewThresholdBP *int32      `json:"review_threshold_bp"`
	MaxDriftBP        *int32      `json:"max_drift_bp"`
	DriftPolicy       DriftPolicy `json:"drift_policy,omitempty"`
}

// Validate checks required fields and basis-point ranges, and defaults
// drift_policy to BLOCK (fail closed).
func (r *RegisterAnomalyModelRequest) Validate() error {
	if err := requireShortText("domain_name", r.DomainName, 128); err != nil {
		return err
	}
	if err := requireShortText("model_provider", r.ModelProvider, 128); err != nil {
		return err
	}
	if err := requireShortText("model_version", r.ModelVersion, 128); err != nil {
		return err
	}
	if err := requireBasisPoints("review_threshold_bp", r.ReviewThresholdBP); err != nil {
		return err
	}
	if err := requireBasisPoints("max_drift_bp", r.MaxDriftBP); err != nil {
		return err
	}
	if r.DriftPolicy == "" {
		r.DriftPolicy = DriftPolicyBlock
	}
	if !r.DriftPolicy.IsValid() {
		return NewValidationError("drift_policy", "drift_policy must be BLOCK, ESCALATE or DISABLE")
	}
	return nil
}

// UpdateAnomalyModelRequest is the body of PUT /v1/anomaly-governance/models/{modelID}.
// It replaces every mutable field; identity fields (domain, provider,
// model_version) never change — register a new model version instead.
type UpdateAnomalyModelRequest struct {
	BodyScope
	ReviewThresholdBP *int32      `json:"review_threshold_bp"`
	MaxDriftBP        *int32      `json:"max_drift_bp"`
	DriftPolicy       DriftPolicy `json:"drift_policy"`
	Status            ModelStatus `json:"status"`
}

func (r *UpdateAnomalyModelRequest) Validate() error {
	if err := requireBasisPoints("review_threshold_bp", r.ReviewThresholdBP); err != nil {
		return err
	}
	if err := requireBasisPoints("max_drift_bp", r.MaxDriftBP); err != nil {
		return err
	}
	if !r.DriftPolicy.IsValid() {
		return NewValidationError("drift_policy", "drift_policy is required: BLOCK, ESCALATE or DISABLE")
	}
	if !r.Status.IsValid() {
		return NewValidationError("status", "status is required: Active or Disabled")
	}
	return nil
}

// ModelFilter narrows GET /v1/anomaly-governance/models.
type ModelFilter struct {
	DomainName string
	Status     ModelStatus
}

// DriftMetrics is the "drift metrics" read surface for one model, plus the
// false-positive feedback the spec asks to be monitored.
type DriftMetrics struct {
	ModelID             string           `json:"model_id"`
	ModelVersion        string           `json:"model_version"`
	MaxDriftBP          int32            `json:"max_drift_bp"`
	DriftPolicy         DriftPolicy      `json:"drift_policy"`
	ModelStatus         ModelStatus      `json:"model_status"`
	RunCount            int64            `json:"run_count"`
	DriftBreachedRuns   int64            `json:"drift_breached_runs"`
	LastObservedDriftBP *int32           `json:"last_observed_drift_bp"`
	MaxObservedDriftBP  *int32           `json:"max_observed_drift_bp"`
	AvgObservedDriftBP  *float64         `json:"avg_observed_drift_bp"`
	SignalCount         int64            `json:"signal_count"`
	SignalsByStatus     map[string]int64 `json:"signals_by_status"`
	ConfirmedCount      int64            `json:"confirmed_count"`
	DismissedCount      int64            `json:"dismissed_count"`
	EscalatedCount      int64            `json:"escalated_count"`
	// FalsePositiveRateBP = dismissed / (confirmed + dismissed), in basis
	// points; null until at least one signal has been closed.
	FalsePositiveRateBP *int64 `json:"false_positive_rate_bp"`
}

// ── DetectionRun ────────────────────────────────────────────────────────────

// DetectionRun is one RunDetection call's immutable provenance. The model's
// thresholds, policy version and publisher are frozen here so the decision
// can be replayed and audited even after the model is reconfigured.
type DetectionRun struct {
	RunID              string      `json:"run_id"`
	TenantID           string      `json:"tenant_id"`
	ModelID            string      `json:"model_id"`
	DomainName         string      `json:"domain_name"`
	ModelProvider      string      `json:"model_provider"`
	ModelVersion       string      `json:"model_version"`
	ModelPolicyVersion int         `json:"model_policy_version"`
	ModelPublishedBy   string      `json:"model_published_by"`
	FeatureSetVersion  string      `json:"feature_set_version"`
	ReviewThresholdBP  int32       `json:"review_threshold_bp"`
	MaxDriftBP         int32       `json:"max_drift_bp"`
	ObservedDriftBP    int32       `json:"observed_drift_bp"`
	DriftPolicy        DriftPolicy `json:"drift_policy"`
	DriftBreached      bool        `json:"drift_breached"`
	BusinessContext    string      `json:"business_context"`
	CreatedAt          time.Time   `json:"created_at"`
	CreatedBy          string      `json:"created_by"`

	// Signals is filled on create and on GET of a single run.
	Signals []GovernedAnomalySignal `json:"signals,omitempty"`
}

// GovernedSignalInput is one already-scored candidate inside RunDetection.
type GovernedSignalInput struct {
	SourceEntityRef string            `json:"source_entity_ref"`
	Severity        Severity          `json:"severity"`
	AnomalyScoreBP  *int32            `json:"anomaly_score_bp"`
	Features        map[string]string `json:"features"`
	// RequestedAction may only be empty or REVIEW (advisory boundary).
	RequestedAction string `json:"requested_action,omitempty"`
}

// MaxSignalsPerRun bounds one request (bounded queue/backpressure NFR).
const MaxSignalsPerRun = 500

// RunDetectionRequest is the body of POST /v1/anomaly-governance/runs.
type RunDetectionRequest struct {
	BodyScope
	DomainName        string                `json:"domain_name"`
	ModelProvider     string                `json:"model_provider"`
	ModelVersion      string                `json:"model_version"`
	FeatureSetVersion string                `json:"feature_set_version"`
	ObservedDriftBP   *int32                `json:"observed_drift_bp"`
	BusinessContext   string                `json:"business_context"`
	Signals           []GovernedSignalInput `json:"signals"`
}

// Validate checks the run header and every signal. A run is all-or-nothing:
// one malformed signal refuses the whole run (nothing is persisted).
func (r *RunDetectionRequest) Validate() error {
	if err := requireShortText("domain_name", r.DomainName, 128); err != nil {
		return err
	}
	if err := requireShortText("model_provider", r.ModelProvider, 128); err != nil {
		return err
	}
	if err := requireShortText("model_version", r.ModelVersion, 128); err != nil {
		return err
	}
	if err := requireShortText("feature_set_version", r.FeatureSetVersion, 128); err != nil {
		return err
	}
	if err := requireBasisPoints("observed_drift_bp", r.ObservedDriftBP); err != nil {
		return err
	}
	if len(r.BusinessContext) > 2000 {
		return NewValidationError("business_context", "business_context must be at most 2000 characters")
	}
	if len(r.Signals) == 0 {
		return NewValidationError("signals", "at least one signal is required")
	}
	if len(r.Signals) > MaxSignalsPerRun {
		return NewValidationError("signals", "at most 500 signals per run")
	}
	for index, signal := range r.Signals {
		if err := validateSignalInput(index, signal); err != nil {
			return err
		}
	}
	return nil
}

func validateSignalInput(index int, signal GovernedSignalInput) error {
	fieldPrefix := "signals[" + strconv.Itoa(index) + "]."
	if strings.TrimSpace(signal.SourceEntityRef) == "" {
		return NewValidationError(fieldPrefix+"source_entity_ref", "source_entity_ref is required")
	}
	if len(signal.SourceEntityRef) > 256 {
		return NewValidationError(fieldPrefix+"source_entity_ref", "source_entity_ref must be at most 256 characters")
	}
	if !signal.Severity.IsValid() {
		return NewValidationError(fieldPrefix+"severity", "severity must be LOW, MEDIUM, HIGH or CRITICAL")
	}
	if err := requireBasisPoints(fieldPrefix+"anomaly_score_bp", signal.AnomalyScoreBP); err != nil {
		return err
	}
	if len(signal.Features) > 100 {
		return NewValidationError(fieldPrefix+"features", "at most 100 features per signal")
	}
	if err := CheckAdvisoryBoundary(fieldPrefix+"requested_action", signal.RequestedAction); err != nil {
		return err
	}
	return nil
}

// ── AnomalySignal ───────────────────────────────────────────────────────────

// SignalStatus is the governed signal lifecycle.
type SignalStatus string

const (
	SignalDetected      SignalStatus = "Detected"
	SignalReviewPending SignalStatus = "ReviewPending"
	SignalConfirmed     SignalStatus = "Confirmed"
	SignalDismissed     SignalStatus = "Dismissed"
	SignalEscalated     SignalStatus = "Escalated"
)

// allowedSignalTransitions is the governed signal state machine:
//
//	Detected ──acknowledge──► ReviewPending ──close──► Confirmed | Dismissed
//	                                       └─escalate─► Escalated
//
// Confirmed, Dismissed and Escalated are terminal. The same graph is
// enforced a second time by the enforce_signal_lifecycle database trigger.
var allowedSignalTransitions = map[SignalStatus][]SignalStatus{
	SignalDetected:      {SignalReviewPending},
	SignalReviewPending: {SignalConfirmed, SignalDismissed, SignalEscalated},
	SignalConfirmed:     {},
	SignalDismissed:     {},
	SignalEscalated:     {},
}

func (s SignalStatus) IsValid() bool {
	_, known := allowedSignalTransitions[s]
	return known
}

// CanTransitionSignal reports whether a signal may move from -> to.
func CanTransitionSignal(from, to SignalStatus) bool {
	for _, allowed := range allowedSignalTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// GovernedAnomalySignal is one detected signal. Model provider/version and
// feature-set version are frozen from the run ("feature/model version is
// retained with each signal"); everything except status/version is
// immutable, and a Dismissed signal keeps its full record as evidence.
type GovernedAnomalySignal struct {
	SignalID          string            `json:"signal_id"`
	TenantID          string            `json:"tenant_id"`
	RunID             string            `json:"run_id"`
	DomainName        string            `json:"domain_name"`
	ModelProvider     string            `json:"model_provider"`
	ModelVersion      string            `json:"model_version"`
	FeatureSetVersion string            `json:"feature_set_version"`
	SourceEntityRef   string            `json:"source_entity_ref"`
	Severity          Severity          `json:"severity"`
	AnomalyScoreBP    int32             `json:"anomaly_score_bp"`
	Features          map[string]string `json:"features"`
	ContentHash       string            `json:"content_hash"`
	Status            SignalStatus      `json:"status"`
	AssuranceState    string            `json:"assurance_state"`
	Version           int               `json:"version"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// SignalFilter narrows GET /v1/anomaly-governance/signals ("signals by
// severity/source/model").
type SignalFilter struct {
	Severity        Severity
	SourceEntityRef string
	DomainName      string
	ModelProvider   string
	ModelVersion    string
	Status          SignalStatus
}

// ── Commands on a signal ────────────────────────────────────────────────────

// Disposition reason codes. A close must say why.
var (
	confirmedReasonCodes = []string{"CONFIRMED_EXCEPTION", "CONTROL_FAILURE", "POLICY_BREACH", "DATA_ERROR_CONFIRMED"}
	dismissedReasonCodes = []string{"FALSE_POSITIVE", "EXPECTED_BEHAVIOUR", "DATA_QUALITY_ISSUE", "DUPLICATE_SIGNAL"}
)

// ReasonCodeEscalated is recorded on the disposition written by EscalateForReview.
const ReasonCodeEscalated = "ESCALATED_FOR_REVIEW"

// CloseSignalRequest is the body of POST /v1/anomaly-governance/signals/{signalID}:close.
type CloseSignalRequest struct {
	BodyScope
	Outcome      SignalStatus `json:"outcome"`
	ReasonCode   string       `json:"reason_code"`
	Notes        string       `json:"notes"`
	EvidenceRefs []string     `json:"evidence_refs,omitempty"`
	// RequestedAction may only be empty or REVIEW (advisory boundary): a
	// Confirmed disposition is evidence, not an instruction.
	RequestedAction string `json:"requested_action,omitempty"`
}

func (r *CloseSignalRequest) Validate() error {
	if err := CheckAdvisoryBoundary("requested_action", r.RequestedAction); err != nil {
		return err
	}
	var allowedCodes []string
	switch r.Outcome {
	case SignalConfirmed:
		allowedCodes = confirmedReasonCodes
	case SignalDismissed:
		allowedCodes = dismissedReasonCodes
	default:
		return NewValidationError("outcome", "outcome must be Confirmed or Dismissed (use :escalate to escalate)")
	}
	if !contains(allowedCodes, r.ReasonCode) {
		return NewValidationError("reason_code", "reason_code for "+string(r.Outcome)+" must be one of "+strings.Join(allowedCodes, ", "))
	}
	if strings.TrimSpace(r.Notes) == "" {
		return NewValidationError("notes", "notes are required: record why the signal was "+string(r.Outcome))
	}
	if len(r.Notes) > 4000 {
		return NewValidationError("notes", "notes must be at most 4000 characters")
	}
	return validateEvidenceRefs(r.EvidenceRefs)
}

// EscalateForReviewRequest is the body of POST /v1/anomaly-governance/signals/{signalID}:escalate.
type EscalateForReviewRequest struct {
	BodyScope
	Reason       string   `json:"reason"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	// RequestedAction may only be empty or REVIEW (advisory boundary).
	RequestedAction string `json:"requested_action,omitempty"`
}

func (r *EscalateForReviewRequest) Validate() error {
	if err := CheckAdvisoryBoundary("requested_action", r.RequestedAction); err != nil {
		return err
	}
	if strings.TrimSpace(r.Reason) == "" {
		return NewValidationError("reason", "reason is required")
	}
	if len(r.Reason) > 4000 {
		return NewValidationError("reason", "reason must be at most 4000 characters")
	}
	return validateEvidenceRefs(r.EvidenceRefs)
}

// AcknowledgeSignalRequest is the optional body of
// POST /v1/anomaly-governance/signals/{signalID}:acknowledge. It carries no
// fields of its own; an empty body or {} is accepted.
type AcknowledgeSignalRequest struct {
	BodyScope
}

func (r *AcknowledgeSignalRequest) Validate() error { return nil }

// ── ReviewDisposition ───────────────────────────────────────────────────────

// ReviewDisposition is the append-only, exactly-once terminal disposition of
// a signal. It is pure evidence: nothing is written to any payment,
// write-off or other domain's table as a side effect.
type ReviewDisposition struct {
	DispositionID string       `json:"disposition_id"`
	TenantID      string       `json:"tenant_id"`
	SignalID      string       `json:"signal_id"`
	Outcome       SignalStatus `json:"outcome"`
	ReasonCode    string       `json:"reason_code"`
	Notes         string       `json:"notes"`
	EvidenceRefs  []string     `json:"evidence_refs"`
	Actor         string       `json:"actor"`
	DecidedAt     time.Time    `json:"decided_at"`
}

// DispositionFilter narrows GET /v1/anomaly-governance/dispositions.
type DispositionFilter struct {
	Outcome SignalStatus
}

// ── small validation helpers ────────────────────────────────────────────────

func requireShortText(field, value string, maxLength int) error {
	if strings.TrimSpace(value) == "" {
		return NewValidationError(field, field+" is required")
	}
	if len(value) > maxLength {
		return NewValidationError(field, field+" must be at most "+strconv.Itoa(maxLength)+" characters")
	}
	return nil
}

func requireBasisPoints(field string, value *int32) error {
	if value == nil {
		return NewValidationError(field, field+" is required (0..10000 basis points)")
	}
	if *value < 0 || *value > 10000 {
		return NewValidationError(field, field+" must be between 0 and 10000 basis points")
	}
	return nil
}

func validateEvidenceRefs(refs []string) error {
	if len(refs) > 50 {
		return NewValidationError("evidence_refs", "at most 50 evidence references")
	}
	for _, ref := range refs {
		if strings.TrimSpace(ref) == "" || len(ref) > 256 {
			return NewValidationError("evidence_refs", "each evidence reference must be 1..256 characters")
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
