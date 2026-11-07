package handler

// HTTP surface for identifier verification (ORG-09 / MDM-03, ZS-SVC-U-001 s14.1).
//
//	POST /v1/identifiers/{id}:verify          VerifyIdentifier: records one observation and applies the rules (200)
//	GET  /v1/identifiers/{id}/verifications   the observations recorded for an identifier, newest first
//
// Mounted only when the service satisfies IdentifierVerificationService, which *registry.Service does.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// IdentifierVerificationService is the identifier-verification half of the service contract.
type IdentifierVerificationService interface {
	VerifyPartyIdentifier(ctx context.Context, identifierID string, req domain.VerifyIdentifierRequest) (*registry.IdentifierVerifyOutcome, error)
	ListIdentifierVerifications(ctx context.Context, identifierID string) ([]domain.IdentifierObservation, error)
}

func registerIdentifierVerificationRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(IdentifierVerificationService); !ok {
		return
	}
	r.Post("/identifiers/{identifierID}:verify", h.VerifyIdentifier)
	r.Get("/identifiers/{identifierID}/verifications", h.ListIdentifierVerifications)
}

func (h *Handler) identifierVerifications() IdentifierVerificationService {
	vs, _ := h.svc.(IdentifierVerificationService)
	return vs
}

// VerifyIdentifier records one observation of an identifier.
func (h *Handler) VerifyIdentifier(w http.ResponseWriter, r *http.Request) {
	var req domain.VerifyIdentifierRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.identifierVerifications().VerifyPartyIdentifier(r.Context(), chi.URLParam(r, "identifierID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListIdentifierVerifications lists the observations of an identifier.
func (h *Handler) ListIdentifierVerifications(w http.ResponseWriter, r *http.Request) {
	out, err := h.identifierVerifications().ListIdentifierVerifications(r.Context(), chi.URLParam(r, "identifierID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// The production service offers identifier verification; if it ever stops, the routes would silently vanish.
var _ IdentifierVerificationService = (*registry.Service)(nil)
