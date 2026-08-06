// Package domain defines the authoritative domain types for
// decision-support-svc.
//
// Per docs/architecture/03-microservices.md §15.5, this service "provides
// governed recommendations at approval and oversight points." The spec is
// deliberately thin here — no owned objects, no published events are listed
// at all, unlike every other service in the catalogue. That absence is
// itself a signal: this service needed real scoping before it could be
// built, not just implementation.
//
// Scope chosen: a recommendation is grounded in real historical data, not
// invented heuristics. Given an action_type + legal_entity_id, this service
// calls governance-decision-log-svc synchronously (GET /v1/decisions,
// filtered by entity+action) — the platform's own immutable record of every
// governance decision ever made — and computes a recommendation from the
// GRANTED/DENIED/ESCALATED ratio of prior decisions in that exact context.
// A human reviewing an approval sees "87% of the last 30 decisions for this
// action on this entity were GRANTED" instead of nothing. This is
// intentionally a v1: a real, defensible baseline (frequency-based
// precedent), not a claim to be doing anything more sophisticated.
//
// Per §15's platform rule, reporting/intelligence services fail degraded,
// not destructive: if governance-decision-log-svc is unreachable, or there
// is no history yet, this service returns a NO_HISTORY recommendation
// rather than an error — it may flag and inform, never block an approval
// flow that doesn't depend on it.
package domain

import "time"

type RecommendedAction string

const (
	RecommendedActionApprove   RecommendedAction = "APPROVE"
	RecommendedActionReject    RecommendedAction = "REJECT"
	RecommendedActionEscalate  RecommendedAction = "ESCALATE"
	RecommendedActionNoHistory RecommendedAction = "NO_HISTORY"
)

// Recommendation is one computed recommendation for a pending governed
// decision. Entity-bound (LegalEntityID), never hard-deleted — it's a
// record of advice given, not a live decision.
type Recommendation struct {
	RecommendationID       string            `json:"recommendation_id"`
	TenantID               string            `json:"tenant_id"`
	LegalEntityID          string            `json:"legal_entity_id"`
	SubjectType            string            `json:"subject_type"` // e.g. "purchase_order", "procurement_case"
	SubjectReference       string            `json:"subject_reference"`
	ActionType             string            `json:"action_type"`
	RecommendedAction      RecommendedAction `json:"recommended_action"`
	ConfidenceScore        float64           `json:"confidence_score"` // 0.0-1.0
	Rationale              string            `json:"rationale"`
	PriorDecisionsSampled  int               `json:"prior_decisions_sampled"`
	RequestedByPrincipalID string            `json:"requested_by_principal_id"`
	CorrelationID          string            `json:"correlation_id"`
	CreatedAt              time.Time         `json:"created_at"`
}

// ── wire types ───────────────────────────────────────────────────────────────

type RequestRecommendationRequest struct {
	LegalEntityID    string `json:"legal_entity_id"`
	SubjectType      string `json:"subject_type"`
	SubjectReference string `json:"subject_reference"`
	ActionType       string `json:"action_type"`
	CorrelationID    string `json:"correlation_id"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRecommendationNotFound = errorString("recommendation not found")
	ErrStoreUnavailable       = errorString("decision support store unavailable")

	ErrAuthorizationDenied     = errorString("authorization denied for this decision support action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")
)
