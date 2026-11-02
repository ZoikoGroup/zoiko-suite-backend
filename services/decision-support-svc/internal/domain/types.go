// Package domain defines the authoritative domain types for
// decision-support-svc.
//
// Per docs/architecture/03-microservices.md §15.5, this service "provides
// governed recommendations at approval and oversight points." That section
// names no owned objects or events, so the service is anchored to the AI
// governance specs instead (see ROUTES.md "Spec anchors"):
//
//   - ZS-SVC-X-001 AIG-04 "Human Oversight, Output Disposition & Decision
//     Boundary": a recommendation is an advisory output that waits for an
//     explicit, recorded human disposition (ACCEPTED / REJECTED /
//     SUPERSEDED, or EXPIRED when the review window closes — NP-40 "never
//     auto-accept"). "ACCEPTED does not itself execute the business action."
//   - Data/Search/Analytics/AI spec §2: "Anomaly scores or AI confidence
//     cannot alone trigger adverse, irreversible or material financial
//     actions" and "Every material AI output retains model/provider
//     version, prompt/template version, retrieval/source references".
//
// Two sources of recommendations are supported:
//
//  1. PRECEDENT_ENGINE (default): this service calls
//     governance-decision-log-svc (GET /v1/decisions filtered by entity and
//     action) and recommends the most frequent prior outcome, with
//     confidence = that outcome's share of the sample. If the log is
//     unreachable or empty the recommendation is NO_HISTORY with zero
//     confidence — intelligence services "fail degraded, not destructive".
//  2. EXTERNAL_MODEL: an upstream AI producer submits its own proposal
//     (action, confidence, rationale, model/prompt versions). This service
//     does not trust it with any authority; it only records it for human
//     disposition, exactly like a precedent-engine output.
package domain

import (
	"encoding/json"
	"time"
)

// RecommendedAction is the advice itself. It is never executed by this
// service.
type RecommendedAction string

const (
	RecommendedActionApprove   RecommendedAction = "APPROVE"
	RecommendedActionReject    RecommendedAction = "REJECT"
	RecommendedActionEscalate  RecommendedAction = "ESCALATE"
	RecommendedActionNoHistory RecommendedAction = "NO_HISTORY"
)

// IsValidForExternalProposal reports whether an external model may propose
// this action. NO_HISTORY is reserved for the precedent engine's own
// "I have nothing to go on" answer.
func (a RecommendedAction) IsValidForExternalProposal() bool {
	switch a {
	case RecommendedActionApprove, RecommendedActionReject, RecommendedActionEscalate:
		return true
	default:
		return false
	}
}

// RecommendationSource says who produced the advice.
type RecommendationSource string

const (
	SourcePrecedentEngine RecommendationSource = "PRECEDENT_ENGINE"
	SourceExternalModel   RecommendationSource = "EXTERNAL_MODEL"
)

// Provenance of the built-in precedent engine. Stored on every row it
// produces so a reviewer (or auditor) can tell exactly which method made
// the advice — "silent model switching is prohibited for material
// workflows" (Data/Search/Analytics/AI spec §2).
const (
	PrecedentEngineModelID      = "decision-support/precedent-frequency"
	PrecedentEngineModelVersion = "1.0.0"
)

// AssuranceStateAdvisory is the only assurance state this service ever
// issues: "Every analytical/search/AI output declares freshness, source
// class and assurance state: certified, controlled projection, provisional,
// advisory or unknown." (Data/Search/Analytics/AI spec §3).
const AssuranceStateAdvisory = "ADVISORY"

// HistoryStatus records what the precedent engine found when it looked for
// prior decisions, so the confidence number can be read in context.
type HistoryStatus string

const (
	HistoryAvailable     HistoryStatus = "AVAILABLE"
	HistoryEmpty         HistoryStatus = "EMPTY"
	HistoryUnavailable   HistoryStatus = "UNAVAILABLE"
	HistoryNotApplicable HistoryStatus = "NOT_APPLICABLE" // external proposals
)

// Recommendation is one advisory output awaiting (or having received) a
// human disposition. Never hard-deleted.
type Recommendation struct {
	RecommendationID string `json:"recommendation_id"`
	TenantID         string `json:"tenant_id"`
	LegalEntityID    string `json:"legal_entity_id"`

	// What the advice is about.
	SubjectType      string `json:"subject_type"` // e.g. "purchase_order"
	SubjectReference string `json:"subject_reference"`
	ActionType       string `json:"action_type"` // the governed action the human will (or will not) take

	// The advice and its explanation.
	RecommendedAction     RecommendedAction `json:"recommended_action"`
	ConfidenceScore       float64           `json:"confidence_score"` // 0.0-1.0, 3 decimal places
	Rationale             string            `json:"rationale"`
	PriorDecisionsSampled int               `json:"prior_decisions_sampled"`
	OutcomeDistribution   map[string]int    `json:"outcome_distribution"`
	HistoryStatus         HistoryStatus     `json:"history_status"`
	EvidenceRefs          []string          `json:"evidence_refs"`

	// Provenance.
	Source         RecommendationSource `json:"source"`
	ModelID        string               `json:"model_id"`
	ModelVersion   string               `json:"model_version"`
	PromptVersion  *string              `json:"prompt_version,omitempty"`
	AssuranceState string               `json:"assurance_state"`

	// Oversight and lifecycle.
	OversightClass              OversightClass `json:"oversight_class"`
	RequiresIndependentReviewer bool           `json:"requires_independent_reviewer"`
	Status                      Status         `json:"status"`
	Version                     int            `json:"version"`
	ExpiresAt                   time.Time      `json:"expires_at"`

	// The human decision, once made.
	DecidedByPrincipalID         *string    `json:"decided_by_principal_id,omitempty"`
	DecidedAt                    *time.Time `json:"decided_at,omitempty"`
	DecisionReason               *string    `json:"decision_reason,omitempty"`
	DownstreamActionRef          *string    `json:"downstream_action_ref,omitempty"`
	ApprovalReference            *string    `json:"approval_reference,omitempty"`
	SupersededByRecommendationID *string    `json:"superseded_by_recommendation_id,omitempty"`
	SupersedesRecommendationID   *string    `json:"supersedes_recommendation_id,omitempty"`

	// DownstreamActionExecuted is always false. It is on the wire so no
	// consumer can misread an ACCEPTED recommendation as an executed action
	// (AIG-04 §7.3, NP-38).
	DownstreamActionExecuted bool `json:"downstream_action_executed"`

	RequestedByPrincipalID string    `json:"requested_by_principal_id"`
	CorrelationID          string    `json:"correlation_id"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// ETag is the strong entity tag for the recommendation's current version.
func (r Recommendation) ETag() string {
	return ETagForVersion(r.Version)
}

// Disposition is one append-only entry in a recommendation's decision
// history (ZS-SVC-X-001 §3.1 OutputDisposition). Creation is recorded too,
// with FromStatus empty.
type Disposition struct {
	DispositionID               string    `json:"disposition_id"`
	TenantID                    string    `json:"tenant_id"`
	RecommendationID            string    `json:"recommendation_id"`
	FromStatus                  *Status   `json:"from_status,omitempty"`
	ToStatus                    Status    `json:"to_status"`
	DecidedByPrincipalID        string    `json:"decided_by_principal_id"`
	Reason                      *string   `json:"reason,omitempty"`
	DownstreamActionRef         *string   `json:"downstream_action_ref,omitempty"`
	ApprovalReference           *string   `json:"approval_reference,omitempty"`
	ReplacementRecommendationID *string   `json:"replacement_recommendation_id,omitempty"`
	ResultingVersion            int       `json:"resulting_version"`
	CorrelationID               string    `json:"correlation_id"`
	RequestID                   *string   `json:"request_id,omitempty"`
	DecidedAt                   time.Time `json:"decided_at"`
}

// ── commands (validated input to the service layer) ──────────────────────────

// CallerContext is who is asking, for which tenant, under which trace. It
// is built only from headers set by gateway-auth-svc and the §4 envelope.
type CallerContext struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string // X-Legal-Entity-Id, may be empty on reads
	CorrelationID string
	RequestID     string
}

// IdempotencyInput is the Idempotency-Key header plus a hash of the
// request it arrived with.
type IdempotencyInput struct {
	Key         string
	RequestHash string
}

// ExternalProposal is an upstream AI producer's own recommendation.
type ExternalProposal struct {
	RecommendedAction RecommendedAction `json:"recommended_action"`
	ConfidenceScore   *float64          `json:"confidence_score"`
	Rationale         string            `json:"rationale"`
	ModelID           string            `json:"model_id"`
	ModelVersion      string            `json:"model_version"`
	PromptVersion     string            `json:"prompt_version"`
}

// CreateRecommendationRequest is the POST /v1/recommendations body.
type CreateRecommendationRequest struct {
	TenantID         string            `json:"tenant_id"`
	LegalEntityID    string            `json:"legal_entity_id"`
	SubjectType      string            `json:"subject_type"`
	SubjectReference string            `json:"subject_reference"`
	ActionType       string            `json:"action_type"`
	CorrelationID    string            `json:"correlation_id"`
	OversightClass   string            `json:"oversight_class"`
	ExpiresAt        *time.Time        `json:"expires_at"`
	EvidenceRefs     []string          `json:"evidence_refs"`
	Proposal         *ExternalProposal `json:"proposal"`
}

// DecisionRequest is the body of accept / reject / expire / supersede.
// Each route uses the subset of fields that applies to it.
type DecisionRequest struct {
	Reason                      string `json:"reason"`
	DownstreamActionRef         string `json:"downstream_action_ref"`
	ApprovalReference           string `json:"approval_reference"`
	ReplacementRecommendationID string `json:"replacement_recommendation_id"`
}

// DecisionCommand is a validated DecisionRequest plus its target.
type DecisionCommand struct {
	RecommendationID string
	TargetStatus     Status
	ExpectedVersion  int
	Request          DecisionRequest
}

// ListFilter narrows GET /v1/recommendations.
type ListFilter struct {
	LegalEntityID    string
	SubjectType      string
	SubjectReference string
	ActionType       string
	Status           Status
	Limit            int
	After            *PageCursor
}

// PageCursor is the keyset position of the last row on the previous page.
type PageCursor struct {
	CreatedAt        time.Time
	RecommendationID string
}

// Page is one page of recommendations.
type Page struct {
	Items      []Recommendation `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// ── persistence write sets ───────────────────────────────────────────────────

// Outbox destinations.
const (
	OutboxDestinationKafka         = "KAFKA"
	OutboxDestinationGovernanceLog = "GOVERNANCE_DECISION_LOG"
)

// OutboxMessage is one message written in the same transaction as the state
// change it announces, and delivered later by the outbox relay.
type OutboxMessage struct {
	OutboxID    string
	TenantID    string
	Destination string
	MessageType string
	MessageKey  string
	Payload     json.RawMessage
}

// IdempotencyRecord stores the response of a write so a retry with the same
// Idempotency-Key returns it verbatim instead of acting twice.
type IdempotencyRecord struct {
	TenantID     string
	Key          string
	Operation    string
	RequestHash  string
	ResourceID   string
	ResponseBody json.RawMessage
}

// NewRecommendationWrite is everything persisted, atomically, when a
// recommendation is created.
type NewRecommendationWrite struct {
	Recommendation Recommendation
	Disposition    Disposition
	Outbox         []OutboxMessage
	Idempotency    IdempotencyRecord
}

// TransitionWrite is everything persisted, atomically, when a recommendation
// changes status. The update only applies if the stored row still has
// ExpectedVersion and FromStatus (optimistic concurrency).
type TransitionWrite struct {
	Updated         Recommendation
	FromStatus      Status
	ExpectedVersion int
	Disposition     Disposition
	Outbox          []OutboxMessage
	Idempotency     IdempotencyRecord

	// Set only on supersede: the replacement gets a back-link, and must
	// still be REVIEW_REQUIRED when the write lands.
	ReplacementID *string
}
