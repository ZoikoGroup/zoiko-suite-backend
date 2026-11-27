package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-quality-svc/internal/domain"
)

// Reliance on a certification and the blast radius of revoking it (ZS-SVC-Z-001 RB-05, DQC-017). Recording that
// something relied on certified evidence is a normal act of a downstream service; reading what relied on it is a
// plain data-quality read.
const ActionRelianceRecord = "DQC_RELIANCE_RECORD"

func registerRelianceRoutes(r chi.Router, h *Handler) {
	r.Post("/runs/{runID}/certification/reliances", h.RecordReliance)
	r.Get("/runs/{runID}/certification/blast-radius", h.GetBlastRadius)
	r.Get("/certification-reliances", h.GetRelianceStanding)
}

// RecordReliance answers 201 for a new reliance and 200 with the existing record when the same service, object
// type and object had already recorded it: the natural key is the idempotency, so no Idempotency-Key is needed.
func (h *Handler) RecordReliance(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionRelianceRecord)
	if !ok {
		return
	}
	var req domain.RecordRelianceRequest
	if _, err := strictBody(r, &req, false); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	got, created, err := h.store.RecordReliance(r.Context(), tenant, chi.URLParam(r, "runID"), principal, req)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.respondJSON(w, status, got)
}

func (h *Handler) GetBlastRadius(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionDQRead)
	if !ok {
		return
	}
	got, err := h.store.GetBlastRadius(r.Context(), tenant, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// GetRelianceStanding is the reverse of the blast radius: the certifications one relying object rests on, and whether any
// has been withdrawn. The three query parameters are all required and no other is accepted, so a misspelt filter can never
// silently widen the answer.
func (h *Handler) GetRelianceStanding(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionDQRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		if (k != "relying_service" && k != "relying_object_type" && k != "relying_object_id") || len(v) != 1 {
			h.respondError(w, http.StatusBadRequest, "unsupported or repeated query parameter "+k)
			return
		}
	}
	subj := domain.RelianceSubject{RelyingService: q.Get("relying_service"), RelyingObjectType: q.Get("relying_object_type"), RelyingObjectID: q.Get("relying_object_id")}
	if err := subj.Validate(); err != nil {
		h.respondFromError(w, err)
		return
	}
	got, err := h.store.GetRelianceStanding(r.Context(), tenant, subj)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}
