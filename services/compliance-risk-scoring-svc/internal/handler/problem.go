package handler

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/problem"
)

// statusForKind is the single place a domain error kind becomes an HTTP status.
var statusForKind = map[domain.ErrorKind]int{
	domain.KindValidation:           http.StatusBadRequest,
	domain.KindUnauthenticated:      http.StatusUnauthorized,
	domain.KindForbidden:            http.StatusForbidden,
	domain.KindNotFound:             http.StatusNotFound,
	domain.KindConflict:             http.StatusConflict,
	domain.KindPreconditionFailed:   http.StatusPreconditionFailed,
	domain.KindUnprocessable:        http.StatusUnprocessableEntity,
	domain.KindPreconditionRequired: http.StatusPreconditionRequired,
	domain.KindUnavailable:          http.StatusServiceUnavailable,
}

// writeError renders err as RFC 9457 problem+json. A *domain.Error keeps its
// code and detail; anything else is an unexpected failure: logged in full,
// reported to the caller as a generic 500 without internals.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if domainErr, ok := domain.AsError(err); ok {
		status, known := statusForKind[domainErr.Kind]
		if !known {
			status = http.StatusInternalServerError
		}
		problem.Write(w, r, status, domainErr.Code, domainErr.Detail, domainErr.Fields)
		return
	}

	h.logger.Error("unexpected error", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Error(err))
	problem.Write(w, r, http.StatusInternalServerError, "internal_error", "an unexpected error occurred", nil)
}

// writeJSON writes a 2xx JSON body.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
