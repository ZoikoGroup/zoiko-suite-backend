package domain

import "time"

// ReviewScope is AUD-09's own durable "what is under review" container —
// one per (engagement, target). Review rounds (assignments, notes,
// sign-offs) all hang off it; a sign-off invalidated by a later protected
// change gets a fresh WorkflowInstance under the SAME scope rather than a
// brand new scope.
type ReviewScope struct {
	ReviewScopeID          string    `json:"review_scope_id"`
	EngagementID           string    `json:"engagement_id"`
	TenantID               string    `json:"tenant_id"`
	TargetType             string    `json:"target_type"`
	TargetID               string    `json:"target_id"`
	InitiatedByPrincipalID string    `json:"initiated_by_principal_id"`
	CreatedAt              time.Time `json:"created_at"`
}

const (
	ReviewTargetWorkpaper  = "WORKPAPER"
	ReviewTargetEngagement = "ENGAGEMENT"
	ReviewTargetFinding    = "FINDING"
)

type ReviewAssignment struct {
	AssignmentID        string    `json:"assignment_id"`
	ReviewScopeID       string    `json:"review_scope_id"`
	TenantID            string    `json:"tenant_id"`
	ReviewerPrincipalID string    `json:"reviewer_principal_id"`
	Role                string    `json:"role"`
	CreatedAt           time.Time `json:"created_at"`
}

type ReviewNote struct {
	ReviewNoteID          string     `json:"review_note_id"`
	ReviewScopeID         string     `json:"review_scope_id"`
	TenantID              string     `json:"tenant_id"`
	RaisedByPrincipalID   string     `json:"raised_by_principal_id"`
	Body                  string     `json:"body"`
	Mandatory             bool       `json:"mandatory"`
	Status                string     `json:"status"`
	ResponseBody          *string    `json:"response_body,omitempty"`
	ResolvedByPrincipalID *string    `json:"resolved_by_principal_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
}

const (
	ReviewNoteOpen      = "OPEN"
	ReviewNoteResponded = "RESPONDED"
	ReviewNoteResolved  = "RESOLVED"
)

// SignOff binds one approval to the target's own content fingerprint at
// signing time — see PgStore.SignOff's own doc comment for how
// invalidation on a later protected change works.
type SignOff struct {
	SignOffID           string     `json:"sign_off_id"`
	ReviewScopeID       string     `json:"review_scope_id"`
	TenantID            string     `json:"tenant_id"`
	WorkflowInstanceID  string     `json:"workflow_instance_id"`
	SignedByPrincipalID string     `json:"signed_by_principal_id"`
	ContentFingerprint  string     `json:"content_fingerprint"`
	Status              string     `json:"status"`
	SignedAt            time.Time  `json:"signed_at"`
	InvalidatedAt       *time.Time `json:"invalidated_at,omitempty"`
	InvalidationReason  *string    `json:"invalidation_reason,omitempty"`
}

const (
	SignOffValid       = "VALID"
	SignOffWithdrawn   = "WITHDRAWN"
	SignOffInvalidated = "INVALIDATED"
)

type QualityReviewRecord struct {
	QualityReviewID        string     `json:"quality_review_id"`
	EngagementID           string     `json:"engagement_id"`
	TenantID               string     `json:"tenant_id"`
	Status                 string     `json:"status"`
	StartedByPrincipalID   string     `json:"started_by_principal_id"`
	StartedAt              time.Time  `json:"started_at"`
	CompletedByPrincipalID *string    `json:"completed_by_principal_id,omitempty"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
}

const (
	QualityReviewInProgress = "IN_PROGRESS"
	QualityReviewCompleted  = "COMPLETED"
)

// ── params ───────────────────────────────────────────────────────────────────

// OpenReviewParams.InitiatedByPrincipalID is the person responsible for
// the target under review (e.g. the workpaper's own preparer) — NOT
// necessarily the caller. "No self-review where prohibited" is checked
// against this value, not against whoever happened to call OpenReview.
type OpenReviewParams struct {
	EngagementID, TenantID, TargetType, TargetID, InitiatedByPrincipalID, CorrelationID string
}

type AssignReviewerParams struct {
	ReviewScopeID, TenantID, ReviewerPrincipalID, Role string
}

type RaiseReviewNoteParams struct {
	ReviewScopeID, TenantID, RaisedByPrincipalID, Body string
	Mandatory                                          bool
}

type RespondToReviewNoteParams struct {
	ReviewNoteID, TenantID, ActorPrincipalID, ResponseBody string
}

type ResolveReviewNoteParams struct {
	ReviewNoteID, TenantID, ActorPrincipalID string
}

type SignOffParams struct {
	ReviewScopeID, TenantID, ActorPrincipalID, ContentFingerprint, CorrelationID string
}

type WithdrawSignOffParams struct {
	SignOffID, TenantID, ActorPrincipalID string
}

type StartQualityReviewParams struct {
	EngagementID, TenantID, ActorPrincipalID, CorrelationID string
}

type CompleteQualityReviewParams struct {
	QualityReviewID, TenantID, ActorPrincipalID, CorrelationID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrReviewScopeNotFound = errorString("review scope not found")
var ErrReviewNoteNotFound = errorString("review note not found")
var ErrReviewNoteInvalidState = errorString("review note is not in a state that permits this action")
var ErrSignOffNotFound = errorString("sign-off not found")
var ErrSignOffInvalidState = errorString("sign-off is not in a state that permits this action")

// ErrReviewNoteMandatoryUnresolved is SignOff's own CAS predicate failure
// — "unresolved mandatory notes block sign-off/release."
var ErrReviewNoteMandatoryUnresolved = errorString("review scope has unresolved mandatory notes")

// ErrReviewAssignmentRequired is "role/assignment checked server-side" —
// the actor attempting SignOff must be a recorded reviewer for this scope.
var ErrReviewAssignmentRequired = errorString("actor is not an assigned reviewer for this review scope")

// ErrSelfReviewNotAllowed is "no self-review where prohibited" —
// AssignReviewer refuses if the proposed reviewer is the same principal
// responsible for the target under review.
var ErrSelfReviewNotAllowed = errorString("the principal responsible for this target may not be assigned as its own reviewer")

var ErrQualityReviewNotFound = errorString("quality review record not found")
var ErrQualityReviewInvalidState = errorString("quality review record is not in a state that permits this action")
