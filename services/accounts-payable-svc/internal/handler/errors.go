package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// apiErr is a fully-described refusal: HTTP status, the legacy `error` name,
// the stable machine-readable `code` (spec section 16) and a human detail.
type apiErr struct {
	Status int
	Name   string
	Code   string
	Detail string
}

func (e *apiErr) Error() string { return e.Name + ": " + e.Detail }

func newErr(status int, name, code, detail string) *apiErr {
	return &apiErr{Status: status, Name: name, Code: code, Detail: detail}
}

type errorResponse struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func writeAPIErr(w http.ResponseWriter, e *apiErr) {
	writeJSON(w, e.Status, errorResponse{Error: e.Name, Code: e.Code, Detail: e.Detail})
}

// writeError keeps the existing (status, error-name, detail) call shape and adds
// the stable code derived from it.
func writeError(w http.ResponseWriter, status int, name, detail string) {
	writeJSON(w, status, errorResponse{Error: name, Code: stableCode(status, name), Detail: detail})
}

// stableCode maps the legacy error names/statuses onto the section 16 codes.
func stableCode(status int, name string) string {
	switch name {
	case "authorization_denied":
		return domain.CodeForbidden
	case "self_approval_not_allowed", "sod_conflict":
		return domain.CodeSoDConflict
	case "duplicate_invoice_number", "invoice_quarantined", "duplicate_unresolved":
		return domain.CodeDuplicateRisk
	case "stale_version":
		return domain.CodeStaleVersion
	case "invoice_held_or_disputed":
		return domain.CodeHeldOrDisputed
	case "tax_unavailable":
		return domain.CodeTaxUnavailable
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return domain.CodeForbidden
	case status == http.StatusNotFound:
		return domain.CodeNotFound
	case status == http.StatusServiceUnavailable:
		return domain.CodeDependencyUnavailable
	case status == http.StatusGone:
		return domain.CodeGone
	default:
		return domain.CodeValidationFailed
	}
}

// mutationErr maps a store/domain error from a state change onto a response.
func (h *Handler) mutationErr(w http.ResponseWriter, err error) {
	var ae *apiErr
	switch {
	case errors.As(err, &ae):
		writeAPIErr(w, ae)
	case errors.Is(err, domain.ErrStaleVersion):
		writeAPIErr(w, newErr(http.StatusConflict, "stale_version", domain.CodeStaleVersion, domain.ErrStaleVersion.Error()))
	case errors.Is(err, domain.ErrInvalidTransition):
		writeAPIErr(w, newErr(http.StatusUnprocessableEntity, "invalid_transition", domain.CodeValidationFailed, err.Error()))
	case errors.Is(err, domain.ErrInvoiceNotFound):
		writeAPIErr(w, newErr(http.StatusNotFound, "invoice_not_found", domain.CodeNotFound, ""))
	case errors.Is(err, domain.ErrInvoiceImmutable):
		writeAPIErr(w, newErr(http.StatusConflict, "invoice_immutable", domain.CodeValidationFailed, err.Error()))
	case errors.Is(err, domain.ErrDuplicateInvoiceNumber):
		writeAPIErr(w, newErr(http.StatusConflict, "duplicate_invoice_number", domain.CodeDuplicateRisk,
			"this vendor already has an invoice with this number for this tenant"))
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeAPIErr(w, newErr(http.StatusBadRequest, "invalid_field", domain.CodeValidationFailed, err.Error()))
	default:
		h.log.Error("store unavailable", zap.Error(err))
		writeAPIErr(w, newErr(http.StatusServiceUnavailable, "store_unavailable", domain.CodeDependencyUnavailable, ""))
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
