package domain

import "fmt"

// ErrorKind classifies a domain error so the HTTP layer can choose a status
// code without string matching. Each kind maps to exactly one status in
// internal/handler/problem.go.
type ErrorKind int

const (
	// KindValidation: the request is well-formed JSON but breaks a domain
	// rule (missing field, unknown enum value, out-of-range number). -> 422
	KindValidation ErrorKind = iota + 1
	// KindNotFound: the resource does not exist in the caller's tenant. -> 404
	KindNotFound
	// KindConflict: the request conflicts with current state (illegal state
	// transition, duplicate natural key, write-once field already set). -> 409
	KindConflict
	// KindForbidden: the caller is authenticated but a business rule forbids
	// the action (segregation of duties, legal-entity mismatch). -> 403
	KindForbidden
	// KindStaleVersion: the If-Match ETag does not match the current version. -> 412
	KindStaleVersion
	// KindIdempotencyMismatch: the Idempotency-Key was already used for a
	// different request. -> 409
	KindIdempotencyMismatch
)

// Error is the single error type every domain/store rule violation uses.
//
//   - Code is the stable lower_snake_case machine code clients branch on.
//   - Message is the human-readable detail.
//   - Field (optional) is the JSON field that caused a validation error.
type Error struct {
	Kind    ErrorKind
	Code    string
	Message string
	Field   string
}

func (e *Error) Error() string { return e.Message }

// NewValidationError builds a 422 error tied to one request field.
func NewValidationError(field, message string) *Error {
	return &Error{Kind: KindValidation, Code: "validation_failed", Message: message, Field: field}
}

// NewNotFoundError builds a 404 error.
func NewNotFoundError(code, message string) *Error {
	return &Error{Kind: KindNotFound, Code: code, Message: message}
}

// NewConflictError builds a 409 error.
func NewConflictError(code, message string) *Error {
	return &Error{Kind: KindConflict, Code: code, Message: message}
}

// NewForbiddenError builds a 403 error.
func NewForbiddenError(code, message string) *Error {
	return &Error{Kind: KindForbidden, Code: code, Message: message}
}

// NewInvalidTransitionError builds the 409 returned when a state machine
// refuses a transition.
func NewInvalidTransitionError(resource, fromStatus, toStatus string) *Error {
	return &Error{
		Kind:    KindConflict,
		Code:    "invalid_transition",
		Message: fmt.Sprintf("%s cannot move from %s to %s", resource, fromStatus, toStatus),
	}
}

// NewStaleVersionError builds the 412 returned when If-Match is stale.
func NewStaleVersionError(expected, current int) *Error {
	return &Error{
		Kind:    KindStaleVersion,
		Code:    "stale_version",
		Message: fmt.Sprintf("If-Match version v%d does not match current version v%d", expected, current),
	}
}

// Fixed errors shared by several call sites.
var (
	ErrForecastNotFound        = NewNotFoundError("forecast_not_found", "forecast not found")
	ErrAssistJobNotFound       = NewNotFoundError("forecast_assist_job_not_found", "forecast assist job not found")
	ErrModelReleaseNotFound    = NewNotFoundError("model_release_not_found", "forecast model release not found")
	ErrPlannerDecisionNotFound = NewNotFoundError("planner_decision_not_found", "no planner decision has been recorded for this job")

	ErrIdempotencyKeyReused = &Error{
		Kind:    KindIdempotencyMismatch,
		Code:    "idempotency_conflict",
		Message: "Idempotency-Key was already used for a different request",
	}
)

// IdempotentReplay is returned by a store command when the same
// Idempotency-Key and the same request body were already processed. It is not
// a failure: the handler re-reads ResourceID and returns it exactly like the
// original success response.
type IdempotentReplay struct {
	ResourceID string
}

func (r *IdempotentReplay) Error() string {
	return "idempotent replay of resource " + r.ResourceID
}

// IdempotencyClaim identifies one write command for replay protection.
//
//   - Key is the caller's Idempotency-Key header.
//   - PrincipalID scopes keys per actor so two users cannot collide.
//   - Operation names the command (e.g. "forecast.approve").
//   - RequestSHA256 fingerprints operation + target + body, so reusing a key
//     for a different request is detected and refused.
type IdempotencyClaim struct {
	Key           string
	PrincipalID   string
	Operation     string
	RequestSHA256 string
}
