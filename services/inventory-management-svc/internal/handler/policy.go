package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST/GET /v1/items/{id}/tracking-policy ──────────────────────────────────

func (h *Handler) SetTrackingPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SetTrackingPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryPolicyAssign); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	p := &domain.TrackingPolicy{
		PolicyVersionID: uuid.NewString(), ItemID: id,
		RequiresLotTracking: req.RequiresLotTracking, RequiresSerialTracking: req.RequiresSerialTracking, RequiresExpiryTracking: req.RequiresExpiryTracking,
		EffectiveFrom: now, CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	if err := h.store.SetTrackingPolicy(r.Context(), p, now); err != nil {
		h.log.Error("failed to set tracking policy", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishInventoryPolicyChanged(r.Context(), getCorrelationID(r), principalID, it.TenantID, it.LegalEntityID, id, "tracking")
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetTrackingPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	p, err := h.store.GetCurrentTrackingPolicy(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "tracking_policy_not_set", "")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── POST/GET /v1/items/{id}/valuation-policy ──────────────────────────────────

// SetValuationPolicyFutureEffective is the spec's own named command —
// see migration 000001's doc comment for why effective_from must be
// strictly later than the moment this command runs, structurally
// enforcing "Retroactive valuation-policy change requires controlled
// migration/revaluation approval."
func (h *Handler) SetValuationPolicyFutureEffective(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SetValuationPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.ValuationMethod {
	case domain.ValuationMethodFIFO, domain.ValuationMethodWeightedAverage, domain.ValuationMethodStandardCost:
	default:
		writeError(w, http.StatusBadRequest, "invalid_valuation_method", "valuation_method must be one of FIFO, WEIGHTED_AVERAGE, STANDARD_COST")
		return
	}
	now := time.Now().UTC()
	effectiveFrom := now
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	if !effectiveFrom.After(now) {
		writeError(w, http.StatusUnprocessableEntity, "not_future_effective", domain.ErrValuationPolicyMustBeFutureEffective.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryPolicyAssign); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	p := &domain.ValuationPolicy{
		PolicyVersionID: uuid.NewString(), ItemID: id, ValuationMethod: req.ValuationMethod,
		EffectiveFrom: effectiveFrom, CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	if err := h.store.SetValuationPolicy(r.Context(), p); err != nil {
		h.log.Error("failed to set valuation policy", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishInventoryPolicyChanged(r.Context(), getCorrelationID(r), principalID, it.TenantID, it.LegalEntityID, id, "valuation")
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetValuationPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	p, err := h.store.GetCurrentValuationPolicy(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "valuation_policy_not_set", "")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── GET /v1/items/{id}/profile-as-of ──────────────────────────────────────────

// GetInventoryProfileAsOf backs the spec's own query of the same name —
// proof that the versioned tracking/valuation policies genuinely preserve
// history (migration 000001's negative path #2).
func (h *Handler) GetInventoryProfileAsOf(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	atParam := r.URL.Query().Get("at")
	at := time.Now().UTC()
	if atParam != "" {
		parsed, err := time.Parse(time.RFC3339, atParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_at", "at must be RFC3339")
			return
		}
		at = parsed
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	it, err := h.store.GetItem(r.Context(), id)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, it.LegalEntityID, actionInventoryItemRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tp, vp, err := h.store.GetProfileAsOf(r.Context(), id, at)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"item_id": id, "as_of": at, "tracking_policy": tp, "valuation_policy": vp,
	})
}
