package domain

import (
	"errors"
	"fmt"
)

// Code is a stable, typed error code. Nine come from the Organization &
// Reference Data contract (spec section 3); NOT_FOUND, FORBIDDEN and
// PERIOD_NOT_FOUND are this service's additions (see SPEC_DEVIATIONS.md).
type Code string

const (
	CodeContextInvalid        Code = "CONTEXT_INVALID"
	CodeVersionConflict       Code = "VERSION_CONFLICT"
	CodeDuplicateCandidate    Code = "DUPLICATE_CANDIDATE" // defined by the contract; no REF-05 path raises it
	CodeInvalidTransition     Code = "INVALID_TRANSITION"
	CodeRuleAmbiguous         Code = "RULE_AMBIGUOUS"
	CodeSourceUnverified      Code = "SOURCE_UNVERIFIED"
	CodeSoDDenied             Code = "SOD_DENIED"
	CodeReferenceRetired      Code = "REFERENCE_RETIRED" // defined by the contract; no REF-05 path raises it
	CodeDependencyUnavailable Code = "DEPENDENCY_UNAVAILABLE"

	// CodeNotFound: an accounting period id (or upstream calendar object) that does not exist.
	CodeNotFound Code = "NOT_FOUND"
	// CodePeriodNotFound: no period covers the entity/date/scope asked about.
	// The posting gate answers this INSTEAD of defaulting to OPEN.
	CodePeriodNotFound Code = "PERIOD_NOT_FOUND"
	// CodeForbidden: authorization-svc denied the action.
	CodeForbidden Code = "FORBIDDEN"
)

// Error is a typed, client-visible failure.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Errf builds a typed error.
func Errf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError extracts a typed error from err's chain.
func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrAuthorizationDenied     = errorString("authorization denied for this period action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrVersionConflictStore is returned by a Tx when an optimistic
	// UPDATE ... WHERE version = $n matched nothing.
	ErrVersionConflictStore = errorString("row version changed concurrently")
)
