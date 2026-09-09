package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/locations ────────────────────────────────────────────────────

func (h *Handler) CreateInventoryLocation(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryLocationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.LocationCode == "" || req.LocationType == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, location_code and location_type are required")
		return
	}
	switch req.LocationType {
	case domain.LocationTypeWarehouse, domain.LocationTypeSite, domain.LocationTypeBin, domain.LocationTypeQuarantineArea, domain.LocationTypeTransit:
	default:
		writeError(w, http.StatusBadRequest, "invalid_location_type", "location_type must be one of WAREHOUSE, SITE, BIN, QUARANTINE_AREA, TRANSIT")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionInventoryLocationManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	var parentLocationID *string
	if req.ParentLocationID != "" {
		parent, err := h.store.GetLocation(r.Context(), req.ParentLocationID)
		if err != nil {
			h.writeLocationErr(w, err)
			return
		}
		if parent.LegalEntityID != req.LegalEntityID {
			writeError(w, http.StatusUnprocessableEntity, "reparent_across_legal_entities", domain.ErrReparentAcrossLegalEntities.Error())
			return
		}
		parentLocationID = &req.ParentLocationID
	}

	l := &domain.InventoryLocation{
		LocationID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		LocationCode: req.LocationCode, LocationType: req.LocationType, Description: req.Description, CustodianEntity: req.CustodianEntity,
		Status: domain.LocationStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateLocation(r.Context(), l, parentLocationID); err != nil {
		if errors.Is(err, domain.ErrDuplicateLocationCode) {
			writeError(w, http.StatusUnprocessableEntity, "duplicate_location_code", err.Error())
			return
		}
		h.log.Error("failed to create inventory location", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishInventoryLocationCreated(r.Context(), getCorrelationID(r), principalID, *l)
	writeJSON(w, http.StatusCreated, l)
}

// ── GET /v1/locations/{id}, GET /v1/locations ────────────────────────────────

func (h *Handler) GetLocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// ListEligibleLocations backs the spec's own ListEligibleLocations query
// — ACTIVE locations only, filtered via ?legal_entity_id=; pass
// ?eligible=false to see every location regardless of status.
func (h *Handler) ListEligibleLocations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	eligibleOnly := r.URL.Query().Get("eligible") != "false"
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionInventoryLocationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListLocations(r.Context(), legalEntityID, eligibleOnly)
	if err != nil {
		h.log.Error("ListEligibleLocations: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.InventoryLocation{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/locations/{id}/activate ──────────────────────────────────────────

func (h *Handler) ActivateLocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ActivateLocation(r.Context(), id, principalID, now); err != nil {
		h.writeLocationErr(w, err)
		return
	}
	l.Status, l.ActivatedAt, l.ActivatedByPrincipalID = domain.LocationStatusActive, &now, &principalID
	h.publisher.PublishInventoryLocationActivated(r.Context(), getCorrelationID(r), principalID, *l)
	writeJSON(w, http.StatusOK, l)
}

// ── POST /v1/locations/{id}/suspend ───────────────────────────────────────────

// SuspendLocation has no reverse command in this v1 — see migration
// 000002's doc comment.
func (h *Handler) SuspendLocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SuspendLocationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.SuspendLocation(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeLocationErr(w, err)
		return
	}
	l.Status, l.SuspendedAt, l.SuspendedByPrincipalID, l.SuspensionReason = domain.LocationStatusSuspended, &now, &principalID, &req.Reason
	h.publisher.PublishInventoryLocationChanged(r.Context(), getCorrelationID(r), principalID, l.TenantID, l.LegalEntityID, id, "suspended")
	writeJSON(w, http.StatusOK, l)
}

// ── POST /v1/locations/{id}/amend ─────────────────────────────────────────────

// AmendLocationMetadata never accepts legal_entity_id or location_type —
// see migration 000002's negative path #1.
func (h *Handler) AmendLocationMetadata(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AmendLocationMetadataRequest
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
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.AmendLocationMetadata(r.Context(), id, req.Description, req.CustodianEntity); err != nil {
		h.writeLocationErr(w, err)
		return
	}
	updated, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	h.publisher.PublishInventoryLocationChanged(r.Context(), getCorrelationID(r), principalID, updated.TenantID, updated.LegalEntityID, id, "metadata")
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/locations/{id}/reparent ──────────────────────────────────────────

// ReparentLocationControlled enforces negative paths #1 ("Physical
// location change silently changes legal owner") and #2 ("Circular
// warehouse/bin hierarchy created") before the store is even asked — the
// store itself re-verifies both as the authoritative check.
func (h *Handler) ReparentLocationControlled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReparentLocationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NewParentLocationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "new_parent_location_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ReparentLocation(r.Context(), id, req.NewParentLocationID, principalID, now); err != nil {
		switch {
		case errors.Is(err, domain.ErrReparentAcrossLegalEntities):
			writeError(w, http.StatusUnprocessableEntity, "reparent_across_legal_entities", err.Error())
		case errors.Is(err, domain.ErrCircularLocationHierarchy):
			writeError(w, http.StatusUnprocessableEntity, "circular_hierarchy", err.Error())
		default:
			h.writeLocationErr(w, err)
		}
		return
	}
	h.publisher.PublishInventoryLocationChanged(r.Context(), getCorrelationID(r), principalID, l.TenantID, l.LegalEntityID, id, "reparented")
	writeJSON(w, http.StatusOK, map[string]string{"location_id": id, "new_parent_location_id": req.NewParentLocationID})
}

// ── POST /v1/locations/{id}/quarantine ────────────────────────────────────────

// SetQuarantineState is the spec's own real toggle — one command that
// both enters AND releases quarantine. Enforces negative path #4,
// "Quarantine released without authority": a release refuses if the
// releasing principal is the same one who set the quarantine.
func (h *Handler) SetQuarantineState(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SetQuarantineStateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationQuarantine); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.SetQuarantine(r.Context(), id, principalID, req.Reason, req.Quarantine, now); err != nil {
		switch {
		case errors.Is(err, domain.ErrSelfQuarantineReleaseNotPermitted):
			writeError(w, http.StatusForbidden, "self_quarantine_release_not_permitted", err.Error())
		case errors.Is(err, domain.ErrLocationNotQuarantined):
			writeError(w, http.StatusUnprocessableEntity, "location_not_quarantined", err.Error())
		default:
			h.writeLocationErr(w, err)
		}
		return
	}
	status := domain.LocationStatusActive
	if req.Quarantine {
		status = domain.LocationStatusQuarantine
		l.Status, l.QuarantinedAt, l.QuarantinedByPrincipalID, l.QuarantineReason = status, &now, &principalID, &req.Reason
		h.publisher.PublishInventoryLocationQuarantined(r.Context(), getCorrelationID(r), principalID, *l)
	} else {
		h.publisher.PublishInventoryLocationChanged(r.Context(), getCorrelationID(r), principalID, l.TenantID, l.LegalEntityID, id, "quarantine_released")
	}
	writeJSON(w, http.StatusOK, map[string]string{"location_id": id, "status": status})
}

// ── POST /v1/locations/{id}/retire ────────────────────────────────────────────

func (h *Handler) RetireLocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RetireLocationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.RetireLocation(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeLocationErr(w, err)
		return
	}
	l.Status, l.RetiredAt, l.RetiredByPrincipalID, l.RetirementReason = domain.LocationStatusRetired, &now, &principalID, &req.Reason
	h.publisher.PublishInventoryLocationRetired(r.Context(), getCorrelationID(r), principalID, *l)
	writeJSON(w, http.StatusOK, l)
}

// ── GET /v1/locations/{id}/hierarchy, /as-of ──────────────────────────────────

// GetLocationHierarchy returns the CURRENT ancestor chain (nearest first).
func (h *Handler) GetLocationHierarchy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	chain, err := h.store.GetAncestorChain(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"location_id": id, "ancestors": chain})
}

// GetLocationAsOf backs the spec's own query of the same name — proof
// that hierarchy changes are versioned, not rewritten (migration 000002's
// own state-model claim).
func (h *Handler) GetLocationAsOf(w http.ResponseWriter, r *http.Request) {
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
	l, err := h.store.GetLocation(r.Context(), id)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionInventoryLocationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	parent, err := h.store.GetParentAsOf(r.Context(), id, at)
	if err != nil {
		h.writeLocationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"location_id": id, "as_of": at, "parent_location_id": parent})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeLocationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrLocationNotFound):
		writeError(w, http.StatusNotFound, "location_not_found", "")
	case errors.Is(err, domain.ErrInvalidLocationTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("inventory location store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
