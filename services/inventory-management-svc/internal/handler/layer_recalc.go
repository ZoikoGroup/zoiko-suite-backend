package handler

import (
	"net/http"
	"strings"
	"time"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/valuation/recalculate ───────────────────────────────────────────

// RecalculateValuation is INV-04's RecalculateValuation command: a read-only,
// deterministic verification that recomputes every cost layer of (item,
// location) from immutable evidence and reports drift. It mutates nothing.
func (h *Handler) RecalculateValuation(w http.ResponseWriter, r *http.Request) {
	var req domain.RecalculateValuationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ItemID == "" || req.LocationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "item_id and location_id are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	item, err := h.store.GetItem(r.Context(), req.ItemID)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryValuationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	res, err := h.store.RecalculateValuation(r.Context(), req.ItemID, req.LocationID)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ── POST /v1/valuation/cost-layers/rebuild ───────────────────────────────────

// RebuildCostLayersControlled is INV-04's RebuildCostLayersControlled command.
// It restores remaining_quantity on layers with QUANTITY drift to the value
// their immutable evidence implies — nothing else. Cost drift is reported,
// never auto-fixed; evidence is never rewritten. Requires a reason and the
// senior valuation-approve action, and is audited append-only.
//
// No checkPeriodOpen and no ledger call: the rebuild has no GL effect (it
// corrects a sub-ledger quantity to its own evidence). If it changes the
// inventory value, value_before/value_after in the response lets the
// reconciliation owner see it; nothing is posted here.
func (h *Handler) RebuildCostLayersControlled(w http.ResponseWriter, r *http.Request) {
	var req domain.RebuildCostLayersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ItemID == "" || req.LocationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "item_id and location_id are required")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason_required", domain.ErrRebuildReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	item, err := h.store.GetItem(r.Context(), req.ItemID)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryValuationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	res, err := h.store.RebuildCostLayers(r.Context(), req.ItemID, req.LocationID, strings.TrimSpace(req.Reason), principalID, time.Now().UTC())
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
