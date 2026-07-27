package domain

import "time"

// ── Review Cycle ──────────────────────────────────────────────────────────────

// ReviewCycle owns an appraisal period for a legal entity.
// cycle_type drives what kind of review this is.
type ReviewCycle struct {
	ReviewCycleID string     `json:"review_cycle_id"`
	TenantID      string     `json:"tenant_id"`
	LegalEntityID string     `json:"legal_entity_id"`
	CycleName     string     `json:"cycle_name"`
	CycleType     string     `json:"cycle_type"`   // ANNUAL | SEMI_ANNUAL | PROBATIONARY | PROJECT_BASED
	StartDate     string     `json:"start_date"`   // YYYY-MM-DD
	EndDate       string     `json:"end_date"`     // YYYY-MM-DD
	CycleStatus   string     `json:"cycle_status"` // DRAFT | ACTIVE | IN_EVALUATION | COMPLETED | ARCHIVED
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type CreateCycleRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	CycleName     string `json:"cycle_name"`
	CycleType     string `json:"cycle_type"` // ANNUAL | SEMI_ANNUAL | PROBATIONARY | PROJECT_BASED
	StartDate     string `json:"start_date"` // YYYY-MM-DD
	EndDate       string `json:"end_date"`   // YYYY-MM-DD
}

type UpdateCycleStatusRequest struct {
	Status string `json:"status"` // ACTIVE | IN_EVALUATION | COMPLETED | ARCHIVED
}

// ── Performance Review ────────────────────────────────────────────────────────

// PerformanceReview is the authoritative record for a single employee's
// appraisal within a ReviewCycle.
// State machine (forward-only, no skipping):
//
//	INITIATED
//	  → SELF_ASSESSMENT_PENDING  (review initiated, awaiting employee input)
//	  → MANAGER_REVIEW_PENDING   (self-assessment submitted, awaiting manager)
//	  → SUBMITTED                (manager submitted evaluation)
//	  → APPROVED                 (gated on authorization-svc; governance_decision_id written)
//	  → COMPLETED                (terminal; completed_at is immutable)
//	  → CANCELLED                (terminal; triggered by employee.terminated event)
type PerformanceReview struct {
	PerformanceReviewID   string                 `json:"performance_review_id"`
	TenantID              string                 `json:"tenant_id"`
	LegalEntityID         string                 `json:"legal_entity_id"`
	ReviewCycleID         string                 `json:"review_cycle_id"`
	EmployeeID            string                 `json:"employee_id"`
	ReviewerPrincipalID   string                 `json:"reviewer_principal_id"`
	ReviewStatus          string                 `json:"review_status"`
	OverallRating         *float64               `json:"overall_rating,omitempty"`
	SelfAssessmentPayload map[string]interface{} `json:"self_assessment_payload,omitempty"`
	ManagerEvalPayload    map[string]interface{} `json:"manager_eval_payload,omitempty"`
	GovernanceDecisionID  *string                `json:"governance_decision_id,omitempty"`
	IdempotencyKey        *string                `json:"idempotency_key,omitempty"`
	CompletedAt           *time.Time             `json:"completed_at,omitempty"`
	EffectiveFrom         time.Time              `json:"effective_from"`
	EffectiveTo           *time.Time             `json:"effective_to,omitempty"`
	CreatedAt             time.Time              `json:"created_at"`
	UpdatedAt             time.Time              `json:"updated_at"`
}

type CreateReviewRequest struct {
	LegalEntityID       string  `json:"legal_entity_id"`
	ReviewCycleID       string  `json:"review_cycle_id"`
	EmployeeID          string  `json:"employee_id"`
	ReviewerPrincipalID string  `json:"reviewer_principal_id"`
	IdempotencyKey      *string `json:"idempotency_key,omitempty"`
}

type SelfAssessmentRequest struct {
	Payload map[string]interface{} `json:"payload"`
}

type ManagerEvaluationRequest struct {
	Payload       map[string]interface{} `json:"payload"`
	OverallRating float64                `json:"overall_rating"` // 0.00–5.00
}

// ── Sentinel Errors ───────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrReviewNotFound          = errorString("performance review not found")
	ErrCycleNotFound           = errorString("review cycle not found")
	ErrInvalidStatusTransition = errorString("invalid review status transition")
	ErrDuplicateIdempotencyKey = errorString("duplicate idempotency key: review already exists")
	ErrAuthorizationDenied     = errorString("authorization denied for performance review action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing from context")
	ErrStoreUnavailable        = errorString("performance review store unavailable")
)
