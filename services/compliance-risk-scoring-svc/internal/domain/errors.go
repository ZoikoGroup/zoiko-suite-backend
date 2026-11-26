package domain

import (
	"errors"
	"fmt"
)

// ErrorKind classifies a business failure. The handler layer maps each kind
// to exactly one HTTP status (see handler/problem.go), so services and the
// domain never deal in status codes directly.
type ErrorKind string

const (
	KindValidation           ErrorKind = "validation"            // 400
	KindUnauthenticated      ErrorKind = "unauthenticated"       // 401
	KindForbidden            ErrorKind = "forbidden"             // 403
	KindNotFound             ErrorKind = "not_found"             // 404
	KindConflict             ErrorKind = "conflict"              // 409
	KindPreconditionFailed   ErrorKind = "precondition_failed"   // 412
	KindUnprocessable        ErrorKind = "unprocessable"         // 422
	KindPreconditionRequired ErrorKind = "precondition_required" // 428
	KindUnavailable          ErrorKind = "unavailable"           // 503
)

// FieldError locates a validation failure inside the request body.
type FieldError struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Error is the single error type returned by domain and service code for
// anything the caller can act on. Code is a stable lower_snake_case value
// clients may branch on.
type Error struct {
	Kind   ErrorKind
	Code   string
	Detail string
	Fields []FieldError
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// AsError extracts a *Error from err, if there is one.
func AsError(err error) (*Error, bool) {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr, true
	}
	return nil, false
}

// Constructors — one per kind, so call sites read as sentences.

func ValidationError(fields []FieldError) *Error {
	return &Error{Kind: KindValidation, Code: "validation_failed", Detail: "request body failed validation", Fields: fields}
}

func BadRequest(code, detail string) *Error {
	return &Error{Kind: KindValidation, Code: code, Detail: detail}
}

func Forbidden(code, detail string) *Error {
	return &Error{Kind: KindForbidden, Code: code, Detail: detail}
}

func NotFound(detail string) *Error {
	return &Error{Kind: KindNotFound, Code: "not_found", Detail: detail}
}

func Conflict(code, detail string) *Error {
	return &Error{Kind: KindConflict, Code: code, Detail: detail}
}

func InvalidTransition(entity, from, action string) *Error {
	return &Error{
		Kind:   KindConflict,
		Code:   "invalid_transition",
		Detail: fmt.Sprintf("%s in status %s cannot be %s", entity, from, action),
	}
}

func Unprocessable(code, detail string) *Error {
	return &Error{Kind: KindUnprocessable, Code: code, Detail: detail}
}

func PreconditionFailed(detail string) *Error {
	return &Error{Kind: KindPreconditionFailed, Code: "version_mismatch", Detail: detail}
}

func PreconditionRequired(detail string) *Error {
	return &Error{Kind: KindPreconditionRequired, Code: "if_match_required", Detail: detail}
}

func Unavailable(code, detail string) *Error {
	return &Error{Kind: KindUnavailable, Code: code, Detail: detail}
}
