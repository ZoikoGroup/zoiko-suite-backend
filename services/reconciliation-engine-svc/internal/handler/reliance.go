package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	customMiddleware "zoiko.io/reconciliation-engine-svc/internal/middleware"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// ActionRelianceRecord is what a downstream service needs to record that it relied on a certification. It is a
// routine act, distinct from certifying, revoking or managing runs. Reading the blast radius is a plain read.
const ActionRelianceRecord = "RECONCILIATION_RELIANCE_RECORD"

// RecordReliance answers 201 for a new reliance and 200 with the existing record when the same service, object type
// and object had already recorded it: the natural key is the idempotency, so no Idempotency-Key is needed.
func (h *Handler) RecordReliance(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.authorizeRun(w, r, ActionRelianceRecord)
	if !ok {
		return
	}
	var req domain.RecordRelianceRequest
	if _, err := h.readStrict(r, &req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	got, created, err := h.store.RecordReliance(r.Context(), tenantID, chiRunID(r), principalID, req)
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
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	got, err := h.store.GetBlastRadius(r.Context(), tenantID, chiRunID(r))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func chiRunID(r *http.Request) string { return chi.URLParam(r, "runID") }

// GetRelianceStanding is the reverse of the blast radius: the certifications of one legal entity that a relying object
// rests on, and whether any has been withdrawn. legal_entity_id and the three relying_* parameters are all required, no other
// parameter is accepted (a misspelt filter must never silently widen the answer), and the caller is authorized to read on that
// entity.
func (h *Handler) GetRelianceStanding(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		switch k {
		case "legal_entity_id", "relying_service", "relying_object_type", "relying_object_id":
		default:
			h.respondError(w, http.StatusBadRequest, "unsupported query parameter "+k)
			return
		}
		if len(v) != 1 {
			h.respondError(w, http.StatusBadRequest, "repeated query parameter "+k)
			return
		}
	}
	entity := q.Get("legal_entity_id")
	if !domain.ValidUUID(entity) {
		h.respondError(w, http.StatusBadRequest, "legal_entity_id must be a UUID")
		return
	}
	if !h.authorize(w, r, principalID, entity, ActionReconciliationRead) {
		return
	}
	subj := domain.RelianceSubject{RelyingService: q.Get("relying_service"), RelyingObjectType: q.Get("relying_object_type"), RelyingObjectID: q.Get("relying_object_id")}
	if err := subj.Validate(); err != nil {
		h.respondFromError(w, err)
		return
	}
	got, err := h.store.GetRelianceStanding(r.Context(), tenantID, entity, subj)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}
