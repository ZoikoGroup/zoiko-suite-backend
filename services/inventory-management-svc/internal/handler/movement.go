package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/movements ───────────────────────────────────────────────────────

// CreateInventoryMovement is the one real create path — see migration
// 000003's doc comment. ReceiveInventory/IssueInventory/TransferInventory/
// AdjustInventoryFromApprovedCount are thin wrappers that pre-set
// MovementType and call the same internal logic.
func (h *Handler) CreateInventoryMovement(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryMovementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	h.createMovement(w, r, req, actionInventoryMovementCreate)
}

func (h *Handler) createMovement(w http.ResponseWriter, r *http.Request, req domain.CreateInventoryMovementRequest, action string) {
	if req.ItemID == "" || req.UOM == "" || req.SourceReference == "" || req.SourceIdempotencyKey == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "item_id, uom, source_reference, source_idempotency_key and fiscal_period are required")
		return
	}
	if req.Quantity <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_quantity", "quantity must be positive")
		return
	}
	if req.MovementType == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrMovementTypeRequired.Error())
		return
	}

	switch req.MovementType {
	case domain.MovementTypeReceipt:
		if req.DestinationLocationID == "" || req.SourceLocationID != "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_movement_shape", domain.ErrReceiptRequiresDestinationOnly.Error())
			return
		}
	case domain.MovementTypeIssue:
		if req.SourceLocationID == "" || req.DestinationLocationID != "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_movement_shape", domain.ErrIssueRequiresSourceOnly.Error())
			return
		}
	case domain.MovementTypeTransfer:
		if req.SourceLocationID == "" || req.DestinationLocationID == "" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_movement_shape", domain.ErrTransferRequiresBothLocations.Error())
			return
		}
	case domain.MovementTypeAdjustment:
		if (req.SourceLocationID == "") == (req.DestinationLocationID == "") {
			writeError(w, http.StatusUnprocessableEntity, "invalid_movement_shape", domain.ErrAdjustmentRequiresOneLocation.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid_movement_type", "movement_type must be one of RECEIPT, ISSUE, TRANSFER, ADJUSTMENT")
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

	item, err := h.store.GetItem(r.Context(), req.ItemID)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	businessDate := time.Now().UTC()
	if req.BusinessDate != nil {
		businessDate = *req.BusinessDate
	}
	m := &domain.InventoryMovement{
		MovementID: uuid.NewString(), TenantID: tenantID, LegalEntityID: item.LegalEntityID,
		MovementType: req.MovementType, Status: domain.MovementStatusDraft,
		ItemID: req.ItemID, Quantity: req.Quantity, UOM: req.UOM,
		SourceReference: req.SourceReference, SourceIdempotencyKey: req.SourceIdempotencyKey,
		BusinessDate: businessDate, FiscalPeriod: req.FiscalPeriod,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if req.SourceLocationID != "" {
		m.SourceLocationID = &req.SourceLocationID
	}
	if req.DestinationLocationID != "" {
		m.DestinationLocationID = &req.DestinationLocationID
	}
	if req.LotNumber != "" {
		m.LotNumber = &req.LotNumber
	}
	if req.SerialNumber != "" {
		m.SerialNumber = &req.SerialNumber
	}
	if req.Reason != "" {
		m.Reason = &req.Reason
	}

	if err := h.store.CreateMovement(r.Context(), m); err != nil {
		h.log.Error("failed to create inventory movement", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// ── POST /v1/movements/receive, /issue, /transfer, /adjust ──────────────────
//
// Type-specific convenience entry points the spec names as their own
// explicit commands — each pre-sets MovementType and funnels into the
// shared createMovement path. AdjustInventoryFromApprovedCount alone
// uses actionInventoryMovementAdjust — the structural half of the spec's
// own SoD, "source domain cannot create arbitrary inventory adjustment
// disguised as receipt/issue."

func (h *Handler) ReceiveInventory(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryMovementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.MovementType = domain.MovementTypeReceipt
	h.createMovement(w, r, req, actionInventoryMovementCreate)
}

func (h *Handler) IssueInventory(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryMovementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.MovementType = domain.MovementTypeIssue
	h.createMovement(w, r, req, actionInventoryMovementCreate)
}

func (h *Handler) TransferInventory(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryMovementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.MovementType = domain.MovementTypeTransfer
	h.createMovement(w, r, req, actionInventoryMovementCreate)
}

func (h *Handler) AdjustInventoryFromApprovedCount(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInventoryMovementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.MovementType = domain.MovementTypeAdjustment
	h.createMovement(w, r, req, actionInventoryMovementAdjust)
}

// ── GET /v1/movements/{id}, GET /v1/movements?item_id= ───────────────────────

func (h *Handler) GetMovement(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	m, err := h.store.GetMovement(r.Context(), id)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, m.LegalEntityID, actionInventoryMovementRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ListMovements backs the spec's own ListMovements/GetMovementChain
// queries — every movement recorded against one item, most recent first.
func (h *Handler) ListMovements(w http.ResponseWriter, r *http.Request) {
	itemID := r.URL.Query().Get("item_id")
	if itemID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "item_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	item, err := h.store.GetItem(r.Context(), itemID)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryMovementRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListMovements(r.Context(), itemID)
	if err != nil {
		h.log.Error("ListMovements: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.InventoryMovement{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/movements/{id}/validate ─────────────────────────────────────────

func (h *Handler) ValidateMovement(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	m, err := h.store.GetMovement(r.Context(), id)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, m.LegalEntityID, actionInventoryMovementCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.ValidateMovement(r.Context(), id, time.Now().UTC()); err != nil {
		h.writeMovementErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"movement_id": id, "status": domain.MovementStatusValidated})
}

// ── POST /v1/movements/{id}/commit ───────────────────────────────────────────

// CommitMovement enforces negative path #4, "Hard-closed movement
// backdated without correction path," via financial-close-svc's real
// period-status check before the store is even asked. Every other
// commit-time negative path (UOM mismatch, serial duplication, negative
// stock) is enforced inside CommitMovement itself.
func (h *Handler) CommitMovement(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	m, err := h.store.GetMovement(r.Context(), id)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, m.LegalEntityID, actionInventoryMovementCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if !h.checkPeriodOpen(w, r, tenantID, m.LegalEntityID, m.FiscalPeriod) {
		return
	}
	now := time.Now().UTC()
	if err := h.store.CommitMovement(r.Context(), id, principalID, now); err != nil {
		h.writeMovementErr(w, err)
		return
	}
	updated, err := h.store.GetMovement(r.Context(), id)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	correlationID := getCorrelationID(r)
	h.publisher.PublishInventoryMovementCommitted(r.Context(), correlationID, principalID, *updated)
	switch updated.MovementType {
	case domain.MovementTypeReceipt:
		h.publisher.PublishInventoryReceived(r.Context(), correlationID, principalID, *updated)
	case domain.MovementTypeIssue:
		h.publisher.PublishInventoryIssued(r.Context(), correlationID, principalID, *updated)
	case domain.MovementTypeTransfer:
		h.publisher.PublishInventoryTransferred(r.Context(), correlationID, principalID, *updated)
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/movements/{id}/reverse, /supersede ──────────────────────────────

// ReverseMovement/SupersedeMovement never touch the original row —
// migration 000003's own append-only guard makes that structurally
// impossible for a COMMITTED movement — each creates and commits a brand
// new, mechanically-inverse movement in one step.
func (h *Handler) ReverseMovement(w http.ResponseWriter, r *http.Request) {
	h.correctMovement(w, r, false)
}

func (h *Handler) SupersedeMovement(w http.ResponseWriter, r *http.Request) {
	h.correctMovement(w, r, true)
}

func (h *Handler) correctMovement(w http.ResponseWriter, r *http.Request, isSupersede bool) {
	id := chi.URLParam(r, "id")
	var req domain.ReverseMovementRequest
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	original, err := h.store.GetMovement(r.Context(), id)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, original.LegalEntityID, actionInventoryMovementReverse); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if !h.checkPeriodOpen(w, r, tenantID, original.LegalEntityID, original.FiscalPeriod) {
		return
	}
	now := time.Now().UTC()
	correction, err := h.store.CreateCorrectionMovement(r.Context(), id, principalID, req.Reason, isSupersede, uuid.NewString(), now)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	h.publisher.PublishInventoryMovementReversed(r.Context(), getCorrelationID(r), principalID, *correction)
	writeJSON(w, http.StatusCreated, correction)
}

// ── GET /v1/on-hand ───────────────────────────────────────────────────────────

// GetOnHand/GetOnHandAsOf are always computed live from committed
// movements — see migration 000003's doc comment. Pass ?at= (RFC3339)
// for GetOnHandAsOf's own real point-in-time behavior.
func (h *Handler) GetOnHand(w http.ResponseWriter, r *http.Request) {
	itemID := r.URL.Query().Get("item_id")
	locationID := r.URL.Query().Get("location_id")
	if itemID == "" || locationID == "" {
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
	item, err := h.store.GetItem(r.Context(), itemID)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryMovementRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	atParam := r.URL.Query().Get("at")
	var onHand float64
	if atParam != "" {
		at, err := time.Parse(time.RFC3339, atParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_at", "at must be RFC3339")
			return
		}
		onHand, err = h.store.GetOnHandAsOf(r.Context(), itemID, locationID, at)
		if err != nil {
			h.writeMovementErr(w, err)
			return
		}
	} else {
		onHand, err = h.store.GetOnHand(r.Context(), itemID, locationID)
		if err != nil {
			h.writeMovementErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"item_id": itemID, "location_id": locationID, "on_hand": strconv.FormatFloat(onHand, 'f', -1, 64),
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) checkPeriodOpen(w http.ResponseWriter, r *http.Request, tenantID, legalEntityID, fiscalPeriod string) bool {
	if h.periodChecker == nil {
		h.log.Error("period checker not configured")
		writeError(w, http.StatusServiceUnavailable, "period_checker_not_configured", "")
		return false
	}
	if err := h.periodChecker.CheckPeriodOpen(r.Context(), tenantID, legalEntityID, fiscalPeriod); err != nil {
		if errors.Is(err, domain.ErrPeriodLocked) {
			writeError(w, http.StatusUnprocessableEntity, "period_locked", err.Error())
			return false
		}
		h.log.Error("period check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "period_check_unavailable", err.Error())
		return false
	}
	return true
}

func (h *Handler) writeMovementErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMovementNotFound):
		writeError(w, http.StatusNotFound, "movement_not_found", "")
	case errors.Is(err, domain.ErrInvalidMovementTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrItemNotEligibleForMovement),
		errors.Is(err, domain.ErrLocationNotEligible),
		errors.Is(err, domain.ErrLocationNotFound),
		errors.Is(err, domain.ErrLotIdentityRequired),
		errors.Is(err, domain.ErrSerialIdentityRequired):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case errors.Is(err, domain.ErrUOMMismatch):
		writeError(w, http.StatusUnprocessableEntity, "uom_mismatch", err.Error())
	case errors.Is(err, domain.ErrSerialAlreadyResident), errors.Is(err, domain.ErrSerialNotAtSourceLocation):
		writeError(w, http.StatusUnprocessableEntity, "serial_duplication", err.Error())
	case errors.Is(err, domain.ErrNegativeStockNotAllowed):
		writeError(w, http.StatusUnprocessableEntity, "negative_stock_not_allowed", err.Error())
	default:
		h.log.Error("inventory movement store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
