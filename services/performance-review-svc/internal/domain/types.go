// Package domain defines the authoritative domain types for
// performance-review-svc.
//
// Per docs/architecture/03-microservices.md §11.10, this service "owns
// review cycles, review records, and governed performance artifacts." It
// does not own employee identity — employee-master-svc does — so every
// review record is created against a real, validated employee, confirmed
// via a synchronous call to employee-master-svc rather than trusting a
// caller-supplied employee_id blindly.
package domain

import "time"

// CycleStatus is a linear chain: OPEN -> CLOSED. Closing a cycle is
// terminal — it does not retroactively invalidate review records already
// completed inside it, matching the doctrine's "no destructive overwrite of
// material history" rule.
type CycleStatus string

const (
	CycleStatusOpen   CycleStatus = "OPEN"
	CycleStatusClosed CycleStatus = "CLOSED"
)

// ReviewCycle is one review period (e.g. "2026 Annual Review") for a legal
// entity. Entity-bound (LegalEntityID), never hard-deleted.
type ReviewCycle struct {
	CycleID              string      `json:"cycle_id"`
	TenantID             string      `json:"tenant_id"`
	LegalEntityID        string      `json:"legal_entity_id"`
	CycleName            string      `json:"cycle_name"`
	PeriodStart          string      `json:"period_start"` // YYYY-MM-DD
	PeriodEnd            string      `json:"period_end"`   // YYYY-MM-DD
	Status               CycleStatus `json:"status"`
	CreatedByPrincipalID string      `json:"created_by_principal_id"`
	CorrelationID        string      `json:"correlation_id"`
	CreatedAt            time.Time   `json:"created_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
	ClosedAt             *time.Time  `json:"closed_at,omitempty"`
}

// ReviewStatus is a linear chain: DRAFT -> SUBMITTED -> COMPLETED. There is
// no rejection/fork here — unlike procurement-workflow-svc's case, a review
// always proceeds to completion once submitted; disagreement over content
// is handled by amendment before submission, not by a reject action.
type ReviewStatus string

const (
	ReviewStatusDraft     ReviewStatus = "DRAFT"
	ReviewStatusSubmitted ReviewStatus = "SUBMITTED"
	ReviewStatusCompleted ReviewStatus = "COMPLETED"
)

// ValidReviewTransitions enumerates the only legal status transitions.
var ValidReviewTransitions = map[ReviewStatus][]ReviewStatus{
	ReviewStatusDraft:     {ReviewStatusSubmitted},
	ReviewStatusSubmitted: {ReviewStatusCompleted},
	ReviewStatusCompleted: {},
}

// ReviewRecord is one employee's review within a cycle. Entity-bound
// (LegalEntityID), never hard-deleted.
type ReviewRecord struct {
	ReviewID             string       `json:"review_id"`
	TenantID             string       `json:"tenant_id"`
	LegalEntityID        string       `json:"legal_entity_id"`
	CycleID              string       `json:"cycle_id"`
	EmployeeID           string       `json:"employee_id"`
	ReviewerPrincipalID  string       `json:"reviewer_principal_id"`
	Rating               *int         `json:"rating,omitempty"` // 1-5, set on submit
	Comments             string       `json:"comments,omitempty"`
	Status               ReviewStatus `json:"status"`
	CreatedByPrincipalID string       `json:"created_by_principal_id"`
	CorrelationID        string       `json:"correlation_id"`
	CreatedAt            time.Time    `json:"created_at"`
	UpdatedAt            time.Time    `json:"updated_at"`
	SubmittedAt          *time.Time   `json:"submitted_at,omitempty"`
	CompletedAt          *time.Time   `json:"completed_at,omitempty"`
}

// ── wire types ───────────────────────────────────────────────────────────────

type CreateCycleRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	CycleName     string `json:"cycle_name"`
	PeriodStart   string `json:"period_start"`
	PeriodEnd     string `json:"period_end"`
	CorrelationID string `json:"correlation_id"`
}

type CreateReviewRequest struct {
	LegalEntityID       string `json:"legal_entity_id"`
	CycleID             string `json:"cycle_id"`
	EmployeeID          string `json:"employee_id"`
	ReviewerPrincipalID string `json:"reviewer_principal_id"`
	CorrelationID       string `json:"correlation_id"`
}

type SubmitReviewRequest struct {
	Rating   int    `json:"rating"`
	Comments string `json:"comments"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrCycleNotFound     = errorString("review cycle not found")
	ErrCycleNotOpen      = errorString("review cycle is not open")
	ErrReviewNotFound    = errorString("review record not found")
	ErrInvalidTransition = errorString("invalid review status transition")
	ErrStoreUnavailable  = errorString("performance review store unavailable")

	ErrAuthorizationDenied     = errorString("authorization denied for this performance review action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other service in this platform.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrEmployeeNotFound / ErrEmployeeServiceUnavailable are returned when
	// employee-master-svc cannot confirm the review's subject employee
	// exists. A review record must never be created against an employee_id
	// that isn't a real, validated employee.
	ErrEmployeeNotFound           = errorString("employee not found in employee-master-svc")
	ErrEmployeeServiceUnavailable = errorString("employee-master-svc unavailable")
)
