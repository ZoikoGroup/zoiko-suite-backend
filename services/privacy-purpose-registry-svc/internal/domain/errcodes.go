package domain

// ErrorCode values are the stable, typed codes every HTTP error response
// carries as `error_code` (handler.writeCoded). Before this, every refusal
// was an untyped {"error": "..."} string, so a caller could only tell "wrong
// field" from "not authorized" by matching prose.
//
// Where §32 of ZS-SVC-W-001 names a code for the condition it is reused
// verbatim with its §32 meaning. §32 has no code for "not found", "malformed
// input", "missing header", "idempotency key reused" and similar transport
// conditions, so — exactly as privacy-consent-svc and tenant-entity-registry-svc
// already do — local symbolic codes are added rather than overloading an
// unrelated PRV-0xx code.
const (
	// §32 codes this service produces.
	ErrCodePurposeNotRegistered = "PRV-001" // PURPOSE_NOT_REGISTERED: unknown/unpublished purpose
	ErrCodeActivityInactive     = "PRV-002" // PROCESSING_ACTIVITY_INACTIVE: no active version at the effective time
	ErrCodeImmutableEvidence    = "PRV-020" // IMMUTABLE_EVIDENCE_CONFLICT: edit/republish of published evidence

	// Local extensions — not in §32.
	ErrCodeNotFound             = "NOT_FOUND"
	ErrCodeValidationFailed     = "VALIDATION_FAILED"
	ErrCodeAuthenticationFailed = "AUTHENTICATION_FAILED"
	ErrCodeAuthorizationDenied  = "AUTHORIZATION_DENIED"
	ErrCodeAuthzUnavailable     = "AUTHORIZATION_SERVICE_UNAVAILABLE"
	ErrCodeIdempotencyRequired  = "IDEMPOTENCY_KEY_REQUIRED"
	ErrCodeIdempotencyMismatch  = "IDEMPOTENCY_MISMATCH"
	ErrCodeCorrelationRequired  = "CORRELATION_ID_REQUIRED"
	ErrCodeTenantScopeRequired  = "TENANT_SCOPE_REQUIRED"
	ErrCodeTenantScopeMismatch  = "TENANT_SCOPE_MISMATCH"
	ErrCodePlatformScope        = "PLATFORM_SCOPE_REQUIRED" // record scope and request scope do not match
	ErrCodeInvalidTransition    = "INVALID_TRANSITION"
	ErrCodeSoDDenied            = "SOD_DENIED"
	ErrCodeInvalidAsOf          = "INVALID_AS_OF"
	ErrCodeEffectiveTime        = "EFFECTIVE_TIME_INVALID"
	ErrCodeStoreUnavailable     = "STORE_UNAVAILABLE"
	ErrCodeInternal             = "INTERNAL_ERROR"
)
