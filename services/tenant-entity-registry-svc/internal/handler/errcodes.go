package handler

import (
	"errors"
	"net/http"

	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// Stable typed error codes — ORG shared contract §3: "Use stable typed
// errors: CONTEXT_INVALID, VERSION_CONFLICT, DUPLICATE_CANDIDATE,
// INVALID_TRANSITION, RULE_AMBIGUOUS, SOURCE_UNVERIFIED, SOD_DENIED,
// REFERENCE_RETIRED, DEPENDENCY_UNAVAILABLE."
//
// Every JSON error body carries one as `error_code`. The human-readable
// `error` text is kept beside it and may change; the code may not. Before
// 28 Sep 2026 bodies were free text only, so a client had to parse prose to
// tell a stale version from a duplicate.
//
// The §3 list has no code for "not found", "malformed input", "not
// authorized", idempotency reuse or a server fault, so the last group extends
// it (recorded in SPEC_DEVIATIONS.md). RULE_AMBIGUOUS is used for the one
// resolution this service performs, a tenant's residency region.
const (
	CodeContextInvalid        = "CONTEXT_INVALID"
	CodeVersionConflict       = "VERSION_CONFLICT"
	CodeDuplicateCandidate    = "DUPLICATE_CANDIDATE"
	CodeInvalidTransition     = "INVALID_TRANSITION"
	CodeRuleAmbiguous         = "RULE_AMBIGUOUS"
	CodeSourceUnverified      = "SOURCE_UNVERIFIED"
	CodeSoDDenied             = "SOD_DENIED"
	CodeReferenceRetired      = "REFERENCE_RETIRED"
	CodeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"

	CodeNotFound            = "NOT_FOUND"
	CodeValidationFailed    = "VALIDATION_FAILED"
	CodeAuthorizationDenied = "AUTHORIZATION_DENIED"
	CodeIdempotencyMismatch = "IDEMPOTENCY_MISMATCH"
	// ORG-02 §4.2 server-resolved context refusals (000013).
	CodeJurisdictionRestricted = "JURISDICTION_RESTRICTED"
	CodeNotEntitled            = "NOT_ENTITLED"
	CodeInternal               = "INTERNAL_ERROR"
)

// errorCodes is checked in order: the specific kinds before the broad
// sentinels they refine (ErrVersionConflict is also an ErrConflict).
var errorCodes = []struct {
	err  error
	code string
}{
	{registry.ErrHostTenantMismatch, CodeContextInvalid},
	{registry.ErrUnauthenticated, CodeContextInvalid},
	{registry.ErrVersionConflict, CodeVersionConflict},
	{registry.ErrApprovalFingerprintMismatch, CodeVersionConflict},
	{registry.ErrOnboardingKeyMismatch, CodeIdempotencyMismatch},
	{registry.ErrRegistryConflict, CodeDuplicateCandidate},
	{registry.ErrApprovalPending, CodeDuplicateCandidate},
	{registry.ErrJurisdictionRestricted, CodeJurisdictionRestricted},
	{registry.ErrNotEntitled, CodeNotEntitled},
	{registry.ErrStateConflict, CodeInvalidTransition},
	{registry.ErrInvalidTransition, CodeInvalidTransition},
	{registry.ErrTenantNotTransactable, CodeInvalidTransition},
	{registry.ErrEntityNotOperational, CodeInvalidTransition},
	{registry.ErrApprovalNotPending, CodeInvalidTransition},
	{registry.ErrSelfApproval, CodeSoDDenied},
	{registry.ErrApprovalRequired, CodeSoDDenied},
	{registry.ErrSourceUnverified, CodeSourceUnverified},
	{registry.ErrReferenceInvalid, CodeReferenceRetired},
	{registry.ErrRegionUnresolved, CodeRuleAmbiguous},
	{registry.ErrServiceUnavailable, CodeDependencyUnavailable},
	{registry.ErrUnknownCommand, CodeNotFound},
	{registry.ErrNotFound, CodeNotFound},
	{registry.ErrUnauthorized, CodeAuthorizationDenied},
	{registry.ErrOnboardingKeyRequired, CodeValidationFailed},
	{registry.ErrInvalidInput, CodeValidationFailed},
	{registry.ErrConflict, CodeDuplicateCandidate},
}

// errorCodeFor is the typed code for a service error; ok is false for an
// error this table does not know, which the caller reports as a 500.
func errorCodeFor(err error) (string, bool) {
	for _, e := range errorCodes {
		if errors.Is(err, e.err) {
			return e.code, true
		}
	}
	return "", false
}

// codeForStatus is the code for a refusal written without a service error —
// a malformed body, a bad query parameter.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return CodeValidationFailed
	case http.StatusUnauthorized:
		return CodeContextInvalid
	case http.StatusForbidden:
		return CodeAuthorizationDenied
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeDuplicateCandidate
	case http.StatusServiceUnavailable:
		return CodeDependencyUnavailable
	default:
		return CodeInternal
	}
}
