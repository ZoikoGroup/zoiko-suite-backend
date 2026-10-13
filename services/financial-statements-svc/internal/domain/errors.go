package domain

import (
	"errors"
	"fmt"
)

// Code is a stable, typed error code returned to callers.
type Code string

const (
	CodeContextInvalid        Code = "CONTEXT_INVALID"
	CodeVersionConflict       Code = "VERSION_CONFLICT"
	CodeInvalidTransition     Code = "INVALID_TRANSITION"
	CodeSourceUnverified      Code = "SOURCE_UNVERIFIED"
	CodeSoDDenied             Code = "SOD_DENIED"
	CodeDependencyUnavailable Code = "DEPENDENCY_UNAVAILABLE"
	CodeValidationFailed      Code = "VALIDATION_FAILED"
	CodeNotFound              Code = "NOT_FOUND"
	CodeForbidden             Code = "FORBIDDEN"
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
	ErrAuthorizationDenied     = errorString("authorization denied for this statement set action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrVersionConflictStore is returned by the store when an optimistic
	// UPDATE ... WHERE version = $n matched nothing.
	ErrVersionConflictStore = errorString("row version changed concurrently")
)
