// Package errors provides stable error codes for obligations-svc.
// Per platform standard O7: "Stable error vocabularies (LEGAL_CONTEXT_MISSING...)
// are not used anywhere; responses are {"error": "<free text>"}."
// This package defines a stable error vocabulary with codes that can be
// programmatically handled by clients.
package errors

// ErrorCode represents a stable error code for programmatic handling.
type ErrorCode string

// Standard error codes (aligned with platform-wide vocabulary where applicable)
const (
	// Authentication/Authorization
	ErrCodeIdentityMissing           ErrorCode = "IDENTITY_MISSING"
	ErrCodeTenantMissing             ErrorCode = "TENANT_MISSING"
	ErrCodeForbidden                 ErrorCode = "FORBIDDEN"
	ErrCodeAuthorizationUnavailable  ErrorCode = "AUTHORIZATION_UNAVAILABLE"
	ErrCodeCreatedByMismatch         ErrorCode = "CREATED_BY_MISMATCH"
	ErrCodeLegalEntityMismatch       ErrorCode = "LEGAL_ENTITY_MISMATCH"

	// Validation
	ErrCodeMissingField              ErrorCode = "MISSING_FIELD"
	ErrCodeInvalidField              ErrorCode = "INVALID_FIELD"
	ErrCodeInvalidJSON               ErrorCode = "INVALID_JSON"
	ErrCodeUnknownField              ErrorCode = "UNKNOWN_FIELD"
	ErrCodeRequestTooLarge           ErrorCode = "REQUEST_TOO_LARGE"
	ErrCodeInvalidLimit              ErrorCode = "INVALID_LIMIT"
	ErrCodeInvalidOffset             ErrorCode = "INVALID_OFFSET"
	ErrCodeInvalidIfMatch            ErrorCode = "INVALID_IF_MATCH"
	ErrCodeInvalidIdentifier         ErrorCode = "INVALID_IDENTIFIER"
	ErrCodeEnvelopeIncomplete        ErrorCode = "ENVELOPE_INCOMPLETE"

	// Not Found
	ErrCodeObligationNotFound        ErrorCode = "OBLIGATION_NOT_FOUND"
	ErrCodeJurisdictionNotFound      ErrorCode = "JURISDICTION_NOT_FOUND"

	// Conflict
	ErrCodeConflict                  ErrorCode = "CONFLICT"
	ErrCodeObligationConflict        ErrorCode = "OBLIGATION_CONFLICT"
	ErrCodeIdempotencyKeyConflict    ErrorCode = "IDEMPOTENCY_KEY_CONFLICT"
	ErrCodeWorkflowAlreadyLinked     ErrorCode = "WORKFLOW_ALREADY_LINKED"
	ErrCodeWorkflowNotLinked         ErrorCode = "WORKFLOW_NOT_LINKED"

	// State Machine
	ErrCodeInvalidTransition         ErrorCode = "INVALID_TRANSITION"
	ErrCodeVersionMismatch           ErrorCode = "VERSION_MISMATCH"

	// External Service
	ErrCodeJurisdictionServiceUnavailable ErrorCode = "JURISDICTION_SERVICE_UNAVAILABLE"

	// Store
	ErrCodeStoreUnavailable          ErrorCode = "STORE_UNAVAILABLE"

	// Idempotency
	ErrCodeIdempotencyReplay         ErrorCode = "IDEMPOTENCY_REPLAY"
)

// Error represents a structured error with a stable code.
type Error struct {
	Code    ErrorCode `json:"error"`
	Detail  string    `json:"detail,omitempty"`
	Field   string    `json:"field,omitempty"`
	Service string    `json:"service,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Detail != "" {
		return string(e.Code) + ": " + e.Detail
	}
	return string(e.Code)
}

// New creates a new Error with the given code.
func New(code ErrorCode) *Error {
	return &Error{Code: code}
}

// NewDetail creates a new Error with the given code and detail.
func NewDetail(code ErrorCode, detail string) *Error {
	return &Error{Code: code, Detail: detail}
}

// NewField creates a new Error with the given code and field.
func NewField(code ErrorCode, field string) *Error {
	return &Error{Code: code, Field: field}
}

// NewDetailField creates a new Error with code, detail, and field.
func NewDetailField(code ErrorCode, detail, field string) *Error {
	return &Error{Code: code, Detail: detail, Field: field}
}

// HasCode checks if the error matches the given code. Not named Is: go vet
// requires an Is method to have the errors.Is signature Is(error) bool.
func (e *Error) HasCode(code ErrorCode) bool {
	return e != nil && e.Code == code
}

// Map of domain error messages to stable error codes.
// Uses string matching since we can't import domain errors directly
// (would create circular dependency).
var DomainErrorCodeMap = map[string]ErrorCode{
	"identity missing":                              ErrCodeIdentityMissing,
	"tenant scope missing":                          ErrCodeTenantMissing,
	"forbidden":                                     ErrCodeForbidden,
	"not authorized for this obligation action":     ErrCodeForbidden,
	"authorization service unavailable":             ErrCodeAuthorizationUnavailable,
	"missing field":                                 ErrCodeMissingField,
	"invalid field":                                 ErrCodeInvalidField,
	"invalid JSON":                                  ErrCodeInvalidJSON,
	"unknown field":                                 ErrCodeUnknownField,
	"request too large":                             ErrCodeRequestTooLarge,
	"invalid limit":                                 ErrCodeInvalidLimit,
	"invalid offset":                                ErrCodeInvalidOffset,
	"invalid If-Match header":                       ErrCodeInvalidIfMatch,
	"identifier is not a valid UUID":                ErrCodeInvalidIdentifier,
	"envelope incomplete":                           ErrCodeEnvelopeIncomplete,
	"obligation not found":                          ErrCodeObligationNotFound,
	"jurisdiction not found":                        ErrCodeJurisdictionNotFound,
	"conflict: record already exists with differing attributes": ErrCodeConflict,
	"obligation_conflict":                           ErrCodeObligationConflict,
	"idempotency key conflict: key already used with different request": ErrCodeIdempotencyKeyConflict,
	"workflow already linked":                       ErrCodeWorkflowAlreadyLinked,
	"workflow not linked":                           ErrCodeWorkflowNotLinked,
	"invalid obligation status transition":          ErrCodeInvalidTransition,
	"version mismatch: the resource has been modified by another request": ErrCodeVersionMismatch,
	"jurisdiction-rules-svc unavailable":            ErrCodeJurisdictionServiceUnavailable,
	"obligations store unavailable":                 ErrCodeStoreUnavailable,
}

// GetCode returns the stable error code for a domain error, or empty string if not mapped.
func GetCode(err error) ErrorCode {
	if err == nil {
		return ""
	}
	if code, ok := DomainErrorCodeMap[err.Error()]; ok {
		return code
	}
	return ""
}