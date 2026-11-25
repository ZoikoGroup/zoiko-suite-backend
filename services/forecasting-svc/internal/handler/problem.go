package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/forecasting-svc/internal/authz"
	"zoiko.io/forecasting-svc/internal/domain"
)

// Every error response is RFC 9457 application/problem+json with the Zoiko
// extensions from the Global API Command & Integration Contract §12: a stable
// `code`, `request_id`, `correlation_id`, `retryable` and field-level `errors`.

const problemTypeBase = "https://api.zoikosuite.com/problems/"

// Problem is the error body.
type Problem struct {
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	Status        int            `json:"status"`
	Detail        string         `json:"detail,omitempty"`
	Instance      string         `json:"instance,omitempty"`
	Code          string         `json:"code"`
	RequestID     string         `json:"request_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Retryable     bool           `json:"retryable"`
	Errors        []ProblemField `json:"errors,omitempty"`
}

// ProblemField locates a field-level failure with a JSON Pointer.
type ProblemField struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

var problemTitles = map[int]string{
	http.StatusBadRequest:           "The request is malformed.",
	http.StatusUnauthorized:         "Authentication is required.",
	http.StatusForbidden:            "The request is not permitted.",
	http.StatusNotFound:             "The resource was not found.",
	http.StatusConflict:             "The request conflicts with the current state.",
	http.StatusPreconditionFailed:   "The resource version does not match If-Match.",
	http.StatusUnprocessableEntity:  "The request failed domain validation.",
	http.StatusPreconditionRequired: "An If-Match precondition is required.",
	http.StatusInternalServerError:  "An unexpected error occurred.",
	http.StatusServiceUnavailable:   "A dependency is unavailable.",
}

var problemTypeSlugs = map[int]string{
	http.StatusBadRequest:           "malformed-request",
	http.StatusUnauthorized:         "authentication-required",
	http.StatusForbidden:            "authorization-denied",
	http.StatusNotFound:             "not-found",
	http.StatusConflict:             "concurrency-conflict",
	http.StatusPreconditionFailed:   "stale-version",
	http.StatusUnprocessableEntity:  "domain-validation",
	http.StatusPreconditionRequired: "precondition-required",
	http.StatusInternalServerError:  "internal-error",
	http.StatusServiceUnavailable:   "dependency-unavailable",
}

// writeProblem writes one problem+json response. field is the request field
// the problem is about ("" when it is not field-specific).
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail, field string) {
	problem := Problem{
		Type:          problemTypeBase + problemTypeSlugs[status],
		Title:         problemTitles[status],
		Status:        status,
		Detail:        detail,
		Instance:      r.URL.Path,
		Code:          code,
		RequestID:     r.Header.Get("X-Request-Id"),
		CorrelationID: r.Header.Get("X-Correlation-ID"),
		Retryable:     status == http.StatusServiceUnavailable,
	}
	if field != "" {
		problem.Errors = []ProblemField{{Pointer: "#/" + field, Code: code, Detail: detail}}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// statusForKind maps a domain error kind to its HTTP status.
func statusForKind(kind domain.ErrorKind) int {
	switch kind {
	case domain.KindValidation:
		return http.StatusUnprocessableEntity
	case domain.KindNotFound:
		return http.StatusNotFound
	case domain.KindConflict, domain.KindIdempotencyMismatch:
		return http.StatusConflict
	case domain.KindForbidden:
		return http.StatusForbidden
	case domain.KindStaleVersion:
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

// writeError converts any error from the domain, store or authz client into a
// problem response. Unknown errors are logged and become 500 without leaking
// internal detail.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		writeProblem(w, r, statusForKind(domainErr.Kind), domainErr.Code, domainErr.Message, domainErr.Field)
		return
	}
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeProblem(w, r, http.StatusForbidden, "authorization_denied", "authorization-svc denied this action", "")
		return
	}
	if errors.Is(err, authz.ErrAuthzServiceUnavailable) {
		h.logger.Error("authorization-svc unavailable, failing closed", zap.Error(err))
		writeProblem(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "authorization-svc is unavailable; the request was refused (fail closed)", "")
		return
	}
	h.logger.Error("request failed", zap.String("path", r.URL.Path), zap.Error(err))
	writeProblem(w, r, http.StatusInternalServerError, "internal_error", "an unexpected error occurred", "")
}
