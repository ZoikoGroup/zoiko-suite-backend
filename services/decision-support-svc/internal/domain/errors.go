package domain

import "errors"

// Sentinel errors. The handler maps each one to exactly one HTTP status
// (see handler/problem.go statusForError).
var (
	// 401 — the request never passed gateway-auth-svc verification.
	ErrTenantMissing    = errors.New("X-Tenant-Id header is required")
	ErrPrincipalMissing = errors.New("X-Principal-Id header is required")

	// 403
	ErrTenantMismatch           = errors.New("body tenant_id does not match the X-Tenant-Id header")
	ErrLegalEntityScopeMismatch = errors.New("X-Legal-Entity-Id does not match the recommendation's legal entity")
	ErrAuthorizationDenied      = errors.New("authorization denied for this decision support action")
	ErrSegregationOfDuties      = errors.New("segregation of duties: the principal who requested a material recommendation cannot accept it")

	// 404 — also returned for another tenant's recommendation, so existence
	// is never disclosed across tenants.
	ErrRecommendationNotFound = errors.New("recommendation not found")

	// 409
	ErrIllegalTransition     = errors.New("illegal status transition")
	ErrReviewDeadlinePassed  = errors.New("the review deadline (expires_at) has passed; the recommendation can only be expired")
	ErrReplacementNotPending = errors.New("replacement recommendation is not awaiting review")

	// 412 / 428
	ErrVersionMismatch      = errors.New("If-Match does not match the recommendation's current version")
	ErrPreconditionRequired = errors.New("If-Match header is required for decisions")

	// 422
	ErrIdempotencyKeyReused  = errors.New("Idempotency-Key was already used with a different request")
	ErrOversightProhibited   = errors.New("oversight class O4 (AI prohibited) cannot receive an AI recommendation")
	ErrReplacementNotFound   = errors.New("replacement recommendation not found")
	ErrReplacementMismatched = errors.New("replacement recommendation must be a different recommendation for the same legal entity, subject and action")

	// 503
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")
	ErrStoreUnavailable        = errors.New("decision support store unavailable")

	// Internal: two requests raced on the same Idempotency-Key. The service
	// resolves it by replaying the winner; it never reaches a caller.
	ErrIdempotencyRace = errors.New("idempotency key inserted concurrently")
)

// FieldError is a request-validation failure on one named field (400).
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }

// NewFieldError builds a *FieldError.
func NewFieldError(field, reason string) error {
	return &FieldError{Field: field, Reason: reason}
}

// TransitionError explains an illegal transition (409) with both states.
type TransitionError struct {
	From Status
	To   Status
}

func (e *TransitionError) Error() string {
	return "cannot move a " + string(e.From) + " recommendation to " + string(e.To)
}

// Unwrap lets errors.Is(err, ErrIllegalTransition) match.
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }
