package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-quality-svc/internal/domain"
)

// Revoking a certification is its own permission, apart from certifying and from managing runs
// (ZS-SVC-Z-001 RB-05): withdrawing reliance on certified evidence is a high-risk act.
const ActionCertificationRevoke = "DQC_CERTIFICATION_REVOKE"

func registerRevocationRoutes(r chi.Router, h *Handler) {
	r.Post("/runs/{runID}/certification:revoke", h.RevokeCertification)
}

func (h *Handler) RevokeCertification(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionCertificationRevoke)
	if !ok {
		return
	}
	var req domain.RevokeCertificationRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principal, "RevokeCertification", runID, hash, r.Header.Get("Idempotency-Key"))
	rev, err := h.store.RevokeCertification(r.Context(), tenant, runID, principal, req, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, rev)
}
