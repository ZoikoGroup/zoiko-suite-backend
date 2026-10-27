package domain

// ErrorCode values are the stable, typed codes every HTTP error response now
// carries (handler.writeErrorCoded). Previously every refusal from this
// service was an untyped {"error": "..."} string, so a caller could only
// distinguish "wrong field" from "not authorized" by matching prose — the
// same defect privacy-purpose-registry-svc and tenant-entity-registry-svc
// were found and fixed for.
//
// Where §32 of ZS-SVC-W-001 already names a code for the condition (the
// cross-PRV-service catalogue), it is reused verbatim. The rest extend the
// catalogue the same way tenant-entity-registry-svc's SPEC_DEVIATIONS.md
// documents doing for ORG-02/03: §32 names no code for "not found",
// "malformed input" or "idempotency key reused," so this service adds one
// rather than overloading an unrelated PRV-0xx code.
const (
	// §32 codes this service can actually produce.
	ErrCodePurposeNotRegistered = "PRV-001" // PURPOSE_NOT_REGISTERED
	ErrCodeNoticeVersionInvalid = "PRV-008" // NOTICE_VERSION_INVALID

	// Local extensions — not in §32, following the same naming convention.
	ErrCodeNotFound             = "NOT_FOUND"
	ErrCodeValidationFailed     = "VALIDATION_FAILED"
	ErrCodeAuthenticationFailed = "AUTHENTICATION_FAILED"
	ErrCodeAuthorizationDenied  = "AUTHORIZATION_DENIED"
	ErrCodeAuthzUnavailable     = "AUTHORIZATION_SERVICE_UNAVAILABLE"
	ErrCodeIdempotencyMismatch  = "IDEMPOTENCY_MISMATCH"
	ErrCodeIdempotencyRequired  = "IDEMPOTENCY_KEY_REQUIRED"
	ErrCodeCorrelationRequired  = "CORRELATION_ID_REQUIRED"
	ErrCodeInvalidTransition    = "INVALID_TRANSITION"
	ErrCodeSoDDenied            = "SOD_DENIED"
	ErrCodeAlreadyWithdrawn     = "ALREADY_WITHDRAWN"
	ErrCodeAlreadyInvalidated   = "ALREADY_INVALIDATED"
	ErrCodeTenantScopeRequired  = "TENANT_SCOPE_REQUIRED"
	ErrCodeTenantScopeMismatch  = "TENANT_SCOPE_MISMATCH"
	ErrCodeStoreUnavailable     = "STORE_UNAVAILABLE"
	ErrCodeUpstreamUnavailable  = "UPSTREAM_UNAVAILABLE"
	ErrCodeInternal             = "INTERNAL_ERROR"

	// ErrCodeImportProvenanceRequired: an offline/imported consent (§11.1)
	// was submitted without the provenance the spec requires.
	ErrCodeImportProvenanceRequired = "IMPORT_PROVENANCE_REQUIRED"
)
