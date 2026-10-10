package domain

import (
	"errors"
	"fmt"
)

// Code is a stable, typed error code. The nine shared codes come from the
// Organization & Reference Data contract (spec section 3); NOT_FOUND, FORBIDDEN and TRANSITION_PLAN_REQUIRED are
// this service's additions and are listed in SPEC_DEVIATIONS.md.
type Code string

const (
	CodeContextInvalid        Code = "CONTEXT_INVALID"
	CodeVersionConflict       Code = "VERSION_CONFLICT"
	CodeDuplicateCandidate    Code = "DUPLICATE_CANDIDATE"
	CodeInvalidTransition     Code = "INVALID_TRANSITION"
	CodeRuleAmbiguous         Code = "RULE_AMBIGUOUS" // defined by the contract; no REF-04 path raises it
	CodeSourceUnverified      Code = "SOURCE_UNVERIFIED"
	CodeSoDDenied             Code = "SOD_DENIED"
	CodeReferenceRetired      Code = "REFERENCE_RETIRED"
	CodeDependencyUnavailable Code = "DEPENDENCY_UNAVAILABLE"

	// CodeNotFound: the id does not exist in the caller's tenant, or no calendar version applies (never guessed).
	CodeNotFound Code = "NOT_FOUND"
	// CodeForbidden: authorization-svc denied the action.
	CodeForbidden Code = "FORBIDDEN"

	// CodeTransitionPlanRequired: a calendar change that touches posted or closed
	// history was activated without an approved CalendarTransitionPlan.
	CodeTransitionPlanRequired Code = "TRANSITION_PLAN_REQUIRED"
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
	// ErrAuthorizationDenied / ErrAuthzServiceUnavailable are returned by the
	// authz client (same names as the sibling services' domain packages).
	ErrAuthorizationDenied     = errorString("authorization denied for this fiscal-calendar action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrVersionConflictStore is returned by a Tx when an optimistic
	// UPDATE ... WHERE version = $n matched nothing.
	ErrVersionConflictStore = errorString("row version changed concurrently")
)
