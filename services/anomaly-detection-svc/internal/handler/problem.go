package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/authz"
	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// Problem is an RFC 9457 problem-details body with the Zoiko extensions from
// the Global API contract §12: a stable machine code, the request and
// correlation ids, a retryable hint and per-field errors.
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

// ProblemField points at one offending request field.
type ProblemField struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
}

const problemTypeBase = "https://api.zoikosuite.com/problems/"

// writeProblem renders one problem as application/problem+json.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, fields ...ProblemField) {
	problem := Problem{
		Type:      problemTypeBase + code,
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		Code:      code,
		Retryable: status == http.StatusServiceUnavailable || status == http.StatusInternalServerError,
		Errors:    fields,
	}
	if r != nil {
		problem.Instance = r.URL.Path
		problem.RequestID = r.Header.Get("X-Request-Id")
		problem.CorrelationID = r.Header.Get("X-Correlation-ID")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// statusForKind maps each domain error kind to its one HTTP status.
func statusForKind(kind domain.ErrorKind) int {
	switch kind {
	case domain.KindValidation:
		return http.StatusUnprocessableEntity
	case domain.KindBadRequest:
		return http.StatusBadRequest
	case domain.KindNotFound:
		return http.StatusNotFound
	case domain.KindStateConflict, domain.KindIdempotencyConflict:
		return http.StatusConflict
	case domain.KindPreconditionRequired:
		return http.StatusPreconditionRequired
	case domain.KindStaleVersion:
		return http.StatusPreconditionFailed
	case domain.KindForbidden:
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// writeError renders any error returned by validation, authz or the store.
//
//   - *domain.Error           → its kind's status (4xx) and code
//   - authz denial            → 403 permission_denied
//   - authz unavailable       → 503 authorization_unavailable (fail closed)
//   - anything else           → 500 internal_error, logged, detail hidden
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		status := statusForKind(domainErr.Kind)
		var fields []ProblemField
		if domainErr.Field != "" {
			fields = append(fields, ProblemField{Pointer: "#/" + domainErr.Field, Code: domainErr.Code, Detail: domainErr.Detail})
		}
		writeProblem(w, r, status, domainErr.Code, domainErr.Detail, fields...)
		return
	}

	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeProblem(w, r, http.StatusForbidden, "permission_denied", "authorization-svc denied this action for the caller")
		return
	}
	if errors.Is(err, authz.ErrAuthzServiceUnavailable) {
		h.logger.Error("authorization check failed; failing closed", zap.Error(err))
		writeProblem(w, r, http.StatusServiceUnavailable, "authorization_unavailable",
			"authorization-svc could not be reached; the request was refused (fail closed)")
		return
	}

	h.logger.Error("request failed", zap.String("path", r.URL.Path), zap.Error(err))
	writeProblem(w, r, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
}

// NotFoundHandler and MethodNotAllowedHandler give unknown routes the same
// problem+json shape as every other error.
func NotFoundHandler(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "route_not_found", "no route matches "+r.Method+" "+r.URL.Path)
}

func MethodNotAllowedHandler(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusMethodNotAllowed, "method_not_allowed", r.Method+" is not supported on "+r.URL.Path)
}
