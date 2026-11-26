package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-quality-svc/internal/domain"
)

// Governed risk acceptance (ZS-SVC-Z-001 DQC-05 section 8). Asking and approving are different permissions, and
// approving is the more serious: two people holding it, neither the requester, accept one risk.
const (
	ActionRiskRequest = "DQC_RISK_REQUEST"
	ActionRiskApprove = "DQC_RISK_APPROVE"
)

func registerRiskRoutes(r chi.Router, h *Handler) {
	r.Post("/runs/{runID}/risk-acceptances", h.RequestRiskAcceptance)
	r.Get("/runs/{runID}/risk-acceptances", h.ListRiskAcceptances)
	r.Post("/risk-acceptances/{acceptanceID}:approve", h.ApproveRiskAcceptance)
}

func (h *Handler) RequestRiskAcceptance(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionRiskRequest)
	if !ok {
		return
	}
	var req domain.RequestRiskAcceptanceRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principal, "RequestRiskAcceptance", runID+"/"+req.ResultID, hash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RequestRiskAcceptance(r.Context(), tenant, runID, principal, req, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) ApproveRiskAcceptance(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionRiskApprove)
	if !ok {
		return
	}
	var req domain.ApproveRiskAcceptanceRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	id := chi.URLParam(r, "acceptanceID")
	claim := h.idempotencyClaim(principal, "ApproveRiskAcceptance", id, hash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.ApproveRiskAcceptance(r.Context(), tenant, id, principal, req, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) ListRiskAcceptances(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionDQRead)
	if !ok {
		return
	}
	ks, err := h.store.ListRiskAcceptances(r.Context(), tenant, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, map[string]any{"data": ks, "count": len(ks)})
}
