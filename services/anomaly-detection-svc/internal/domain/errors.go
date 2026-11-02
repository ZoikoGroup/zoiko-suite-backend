package domain

import "fmt"

// ErrorKind classifies a domain failure. The HTTP layer maps each kind to
// exactly one status code (see handler/problem.go), so the store and the
// domain never need to know about HTTP.
type ErrorKind int

const (
	// KindValidation means the request was well-formed JSON but a field
	// failed a business rule. HTTP 422.
	KindValidation ErrorKind = iota + 1

	// KindBadRequest means the request itself is unusable: a malformed id,
	// cursor or query parameter. HTTP 400.
	KindBadRequest

	// KindNotFound means the resource does not exist for this tenant. A row
	// owned by another tenant is reported the same way, so existence is
	// never leaked across tenants. HTTP 404.
	KindNotFound

	// KindStateConflict means the resource exists but its lifecycle state
	// (or a uniqueness rule) does not allow the command. HTTP 409.
	KindStateConflict

	// KindIdempotencyConflict means the Idempotency-Key was already used for
	// a different request body. HTTP 409.
	KindIdempotencyConflict

	// KindPreconditionRequired means a mutation of an existing aggregate was
	// sent without If-Match. HTTP 428.
	KindPreconditionRequired

	// KindStaleVersion means If-Match did not match the current version.
	// HTTP 412.
	KindStaleVersion

	// KindForbidden means the caller is authenticated but the rule forbids
	// this actor from doing this (segregation of duties, scope mismatch).
	// HTTP 403.
	KindForbidden
)

// Error is the single error type the domain and store return for every
// expected (client-caused) failure. Code is a stable machine-readable string
// clients can branch on; Detail is a human-readable sentence; Field, when
// set, names the offending request field.
type Error struct {
	Kind   ErrorKind
	Code   string
	Detail string
	Field  string
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (field %s)", e.Code, e.Detail, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// Is makes errors.Is(err, domain.ErrSignalNotFound) work even when the
// returned error is a copy carrying a more specific Detail: two domain
// errors are "the same" when their Code matches.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Code == other.Code
}

// WithDetail returns a copy of e with a more specific human-readable detail.
func (e *Error) WithDetail(detail string) *Error {
	copied := *e
	copied.Detail = detail
	return &copied
}

// NewValidationError builds a 422 error for one request field.
func NewValidationError(field, detail string) *Error {
	return &Error{Kind: KindValidation, Code: "field_invalid", Field: field, Detail: detail}
}

// NewBadRequestError builds a 400 error for an unusable request part.
func NewBadRequestError(code, field, detail string) *Error {
	return &Error{Kind: KindBadRequest, Code: code, Field: field, Detail: detail}
}

// Sentinel errors. Compare with errors.Is.
var (
	// Generic request problems.
	ErrFieldInvalid = &Error{Kind: KindValidation, Code: "field_invalid", Detail: "a request field is invalid"}
	ErrInvalidID    = &Error{Kind: KindBadRequest, Code: "invalid_id", Detail: "the identifier in the path is not well-formed"}

	// Concurrency and idempotency.
	ErrPreconditionRequired = &Error{Kind: KindPreconditionRequired, Code: "precondition_required",
		Detail: "If-Match is required when changing an existing resource; send the ETag from the last GET"}
	ErrStaleVersion = &Error{Kind: KindStaleVersion, Code: "stale_version",
		Detail: "If-Match does not match the current version of the resource; re-read and retry"}
	ErrIdempotencyKeyReused = &Error{Kind: KindIdempotencyConflict, Code: "idempotency_key_reuse_conflict",
		Detail: "this Idempotency-Key was already used with a different request"}

	// Advisory boundary (DA-CTRL-030, N-19).
	ErrAdvisoryBoundary = &Error{Kind: KindValidation, Code: "advisory_boundary_violation",
		Detail: "an anomaly score or disposition cannot itself trigger a protected action (payment freeze, write-off, ...); " +
			"protected actions require a separate governed command in the owning domain"}

	// Segregation of duties.
	ErrSegregationOfDuties = &Error{Kind: KindForbidden, Code: "sod_violation",
		Detail: "segregation of duties: the reviewer who closes a signal must differ from the detection submitter and the model publisher"}
	ErrActorMismatch = &Error{Kind: KindForbidden, Code: "actor_mismatch",
		Detail: "the actor named in the body must be the authenticated X-Principal-Id"}

	// Rule-engine surface.
	ErrAnomalyRecordNotFound       = &Error{Kind: KindNotFound, Code: "anomaly_record_not_found", Detail: "anomaly record not found"}
	ErrAnomalyRuleNotFound         = &Error{Kind: KindNotFound, Code: "anomaly_rule_not_found", Detail: "anomaly rule not found"}
	ErrAnomalyRuleReferenceInvalid = &Error{Kind: KindValidation, Code: "anomaly_rule_reference_invalid", Field: "rule_id", Detail: "rule_id does not name an active rule of this tenant"}
	ErrInvalidStatusTransition     = &Error{Kind: KindStateConflict, Code: "transition_not_allowed", Detail: "the requested status transition is not allowed"}

	// Governed advisory layer.
	ErrAnomalyModelNotFound      = &Error{Kind: KindNotFound, Code: "anomaly_model_not_found", Detail: "anomaly model not found"}
	ErrAnomalyModelNotRegistered = &Error{Kind: KindValidation, Code: "anomaly_model_not_registered",
		Detail: "no anomaly model is registered for this domain_name/model_provider/model_version"}
	ErrAnomalyModelExists = &Error{Kind: KindStateConflict, Code: "anomaly_model_already_registered",
		Detail: "an anomaly model is already registered for this domain_name/model_provider/model_version; update it with PUT and If-Match"}
	ErrAnomalyModelDisabled = &Error{Kind: KindStateConflict, Code: "anomaly_model_disabled",
		Detail: "the anomaly model is Disabled; re-enable it before running detection"}
	ErrDriftExceedsThreshold = &Error{Kind: KindStateConflict, Code: "drift_threshold_exceeded",
		Detail: "observed drift exceeds the registered anomaly model's max_drift_bp"}
	ErrDetectionRunNotFound = &Error{Kind: KindNotFound, Code: "detection_run_not_found", Detail: "detection run not found"}
	ErrSignalNotFound       = &Error{Kind: KindNotFound, Code: "anomaly_signal_not_found", Detail: "anomaly signal not found"}
	ErrSignalNotDetected    = &Error{Kind: KindStateConflict, Code: "transition_not_allowed", Detail: "anomaly signal is not in Detected status"}
	ErrSignalNotReviewable  = &Error{Kind: KindStateConflict, Code: "transition_not_allowed", Detail: "anomaly signal is not ReviewPending"}
	ErrDispositionNotFound  = &Error{Kind: KindNotFound, Code: "disposition_not_found", Detail: "the signal has no review disposition yet"}
)
