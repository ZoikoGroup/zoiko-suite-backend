package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/events"
)

// ─────────────────────────────────────────────────────────────────────────────
// AIG-04: Human Oversight, Output Disposition & Decision Boundary
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) CreateDisposition(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDispositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UseCaseID == "" || req.ExecutionRef == "" {
		writeError(w, http.StatusBadRequest, "use_case_id and execution_ref are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, "")
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, DispositionCreate) {
		return
	}
	d, err := h.store.CreateDisposition(r.Context(), req, principalID)
	if err != nil {
		h.respondDispositionError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.output_disposition.state_changed", EntityID: d.DispositionID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) GetDisposition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r, ""); !ok {
		return
	}
	if !h.authorize(w, r, principalID, DispositionRead) {
		return
	}
	d, err := h.store.GetDisposition(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.respondDispositionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// DecideDisposition is the only path that can move a disposition to
// ACCEPTED or REJECTED (NP-39: the model's own claimed status is
// never consulted).
func (h *Handler) DecideDisposition(w http.ResponseWriter, r *http.Request) {
	var req domain.DecideDispositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Decision != string(domain.DispositionAccepted) && req.Decision != string(domain.DispositionRejected) {
		writeError(w, http.StatusBadRequest, "decision must be ACCEPTED or REJECTED")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, "")
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, DispositionDecide) {
		return
	}
	d, err := h.store.DecideDisposition(r.Context(), chi.URLParam(r, "id"), req, principalID)
	if err != nil {
		h.respondDispositionError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.output_disposition.state_changed", EntityID: d.DispositionID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) SupersedeDisposition(w http.ResponseWriter, r *http.Request) {
	var req domain.SupersedeDispositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ExecutionRef == "" {
		writeError(w, http.StatusBadRequest, "execution_ref is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, "")
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, DispositionSupersede) {
		return
	}
	d, err := h.store.SupersedeDisposition(r.Context(), chi.URLParam(r, "id"), req, principalID)
	if err != nil {
		h.respondDispositionError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.output_disposition.state_changed", EntityID: d.DispositionID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) respondDispositionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDispositionNotFound), errors.Is(err, domain.ErrUseCaseNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrDispositionNotDecidable), errors.Is(err, domain.ErrDispositionNotSupersedable),
		errors.Is(err, domain.ErrInvalidDispositionDecision), errors.Is(err, domain.ErrReviewerCannotBeSubmitter),
		errors.Is(err, domain.ErrUseCaseNotActiveForOutput):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("output disposition request failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "output disposition request failed")
	}
}
