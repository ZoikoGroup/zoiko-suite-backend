package handler

import (
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/stock-counts ────────────────────────────────────────────────────

func (h *Handler) CreateStockCount(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateStockCountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || len(req.LocationIDs) == 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, fiscal_period and at least one location_id are required")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionInventoryCountPlan); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	sc := &domain.StockCount{
		CountID: uuid.NewString(), LegalEntityID: req.LegalEntityID, FiscalPeriod: req.FiscalPeriod,
		Status: domain.StockCountStatusPlanned, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateStockCount(r.Context(), sc, req.LocationIDs); err != nil {
		h.log.Error("failed to create stock count", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	sc.LocationIDs = req.LocationIDs
	h.publisher.PublishStockCountStarted(r.Context(), getCorrelationID(r), principalID, tenantID, *sc)
	writeJSON(w, http.StatusCreated, sc)
}

// ── GET /v1/stock-counts/{id} ─────────────────────────────────────────────────

// ── GET /v1/stock-counts/unapproved-variance-count ───────────────────────────

// GetUnapprovedVarianceCount is a read-only integrity check — never a
// caller-declared figure. financial-close-svc's ACC-06 calls this
// directly for the AST/INV/PRJ domain spec's own §9 "Stock count"
// assertion; see internal/store's own doc comment for the exact
// calculation and the real gap it surfaces.
func (h *Handler) GetUnapprovedVarianceCount(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	fiscalPeriod := r.URL.Query().Get("fiscal_period")
	if legalEntityID == "" || fiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and fiscal_period are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionInventoryCountRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	count, err := h.store.GetUnapprovedVarianceCount(r.Context(), legalEntityID, fiscalPeriod)
	if err != nil {
		h.log.Error("GetUnapprovedVarianceCount: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"unapproved_variance_count": count})
}

// ── GET /v1/stock-counts/{id} ─────────────────────────────────────────────────

func (h *Handler) GetStockCount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), id)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

// ── POST /v1/stock-counts/{id}/freeze ─────────────────────────────────────────

// FreezeCountPopulation is the spec's own named evidence step — "frozen
// population/watermark" — locking in the real, computed system quantity
// for every (item, location) ever moved through this count's own scoped
// locations, at this exact moment. See migration 000005's doc comment
// for why this snapshot can never change again.
func (h *Handler) FreezeCountPopulation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), id)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountPlan); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	frozenCount, err := h.store.FreezeCountPopulation(r.Context(), id, time.Now().UTC())
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if frozenCount == 0 {
		writeError(w, http.StatusUnprocessableEntity, "empty_count_population", domain.ErrEmptyCountPopulation.Error())
		return
	}
	h.publisher.PublishStockCountPopulationFrozen(r.Context(), getCorrelationID(r), principalID, tenantID, id, frozenCount)
	writeJSON(w, http.StatusOK, map[string]any{"count_id": id, "status": domain.StockCountStatusPopulationFrozen, "frozen_count": frozenCount})
}

// ── POST /v1/stock-counts/lines/{lineID}/assign-counter ──────────────────────

func (h *Handler) AssignCounter(w http.ResponseWriter, r *http.Request) {
	lineID := chi.URLParam(r, "lineID")
	var req domain.AssignCounterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CounterPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "counter_principal_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	line, err := h.store.GetCountLine(r.Context(), lineID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), line.CountID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountPlan); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.AssignCounter(r.Context(), lineID, req.CounterPrincipalID); err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"line_id": lineID, "assigned_counter_principal_id": req.CounterPrincipalID})
}

// ── GET /v1/stock-counts/lines/{lineID} ───────────────────────────────────────

func (h *Handler) GetCountLine(w http.ResponseWriter, r *http.Request) {
	lineID := chi.URLParam(r, "lineID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	line, err := h.store.GetCountLine(r.Context(), lineID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), line.CountID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, line)
}

// ── POST /v1/stock-counts/lines/{lineID}/record-count ─────────────────────────

// RecordBlindCount is the spec's own named command, and negative path
// #1, "Counter sees system quantity in blind count," is why its own
// response type (domain.RecordedBlindCount) has no field for it —
// structurally, not just by convention.
func (h *Handler) RecordBlindCount(w http.ResponseWriter, r *http.Request) {
	lineID := chi.URLParam(r, "lineID")
	var req domain.RecordBlindCountRequest
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
	line, err := h.store.GetCountLine(r.Context(), lineID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), line.CountID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountRecord); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.RecordBlindCount(r.Context(), lineID, principalID, req.ObservedQuantity, time.Now().UTC())
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, domain.RecordedBlindCount{
		LineID: updated.LineID, ItemID: updated.ItemID, LocationID: updated.LocationID,
		ObservedQuantity: *updated.ObservedQuantity, ObservedAt: *updated.ObservedAt,
	})
}

// ── POST /v1/stock-counts/lines/{lineID}/request-recount ─────────────────────

func (h *Handler) RequestRecount(w http.ResponseWriter, r *http.Request) {
	lineID := chi.URLParam(r, "lineID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	line, err := h.store.GetCountLine(r.Context(), lineID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), line.CountID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.RequestRecount(r.Context(), lineID); err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"line_id": lineID, "status": domain.CountLineStatusNeedsRecount})
}

// ── POST /v1/stock-counts/lines/{lineID}/approve-variance ────────────────────

// ApproveCountVariance refuses self-approval — the spec's own SoD,
// "Counter cannot approve own material variance" — enforced at the line
// level inside the store's own guarded transaction.
func (h *Handler) ApproveCountVariance(w http.ResponseWriter, r *http.Request) {
	lineID := chi.URLParam(r, "lineID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	line, err := h.store.GetCountLine(r.Context(), lineID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), line.CountID)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveCountVariance(r.Context(), lineID, principalID, now); err != nil {
		if errors.Is(err, domain.ErrSelfVarianceApprovalNotPermitted) {
			writeError(w, http.StatusForbidden, "self_approval_not_permitted", err.Error())
			return
		}
		h.writeStockCountErr(w, err)
		return
	}
	h.publisher.PublishStockCountVarianceApproved(r.Context(), getCorrelationID(r), principalID, tenantID, lineID)
	writeJSON(w, http.StatusOK, map[string]string{"line_id": lineID, "status": domain.CountLineStatusVarianceApproved})
}

// ── POST /v1/stock-counts/{id}/generate-adjustments ───────────────────────────

// GenerateAdjustmentMovements is the ONLY path from an approved variance
// to an on-hand change — negative path #4, "Count directly overwrites
// on-hand quantity." For every VARIANCE_APPROVED line with a nonzero
// variance, it calls INV-03's own CreateMovement/ValidateMovement/
// CommitMovement in-process (an ADJUSTMENT movement, direction from the
// variance's own sign) — never writing a quantity directly anywhere in
// this capability's own tables.
func (h *Handler) GenerateAdjustmentMovements(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), id)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	generated := 0
	for _, line := range sc.Lines {
		if line.Status != domain.CountLineStatusVarianceApproved || line.ObservedQuantity == nil {
			continue
		}
		variance := *line.ObservedQuantity - line.SystemQuantity
		if variance == 0 {
			continue
		}

		m := &domain.InventoryMovement{
			MovementID: uuid.NewString(), LegalEntityID: sc.LegalEntityID, MovementType: domain.MovementTypeAdjustment, Status: domain.MovementStatusDraft,
			ItemID: line.ItemID, Quantity: math.Abs(variance), UOM: "EACH",
			SourceReference: "STOCK-COUNT-" + id, SourceIdempotencyKey: "count-" + line.LineID, FiscalPeriod: sc.FiscalPeriod,
			BusinessDate: time.Now().UTC(), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
			Reason: strPtrLocal("stock count " + id + " variance approval"),
		}
		if variance > 0 {
			loc := line.LocationID
			m.DestinationLocationID = &loc
		} else {
			loc := line.LocationID
			m.SourceLocationID = &loc
		}
		if err := h.store.CreateMovement(r.Context(), m); err != nil {
			h.log.Error("GenerateAdjustmentMovements: failed to create adjustment movement", zap.String("line_id", line.LineID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		if err := h.store.ValidateMovement(r.Context(), m.MovementID, time.Now().UTC()); err != nil {
			h.log.Error("GenerateAdjustmentMovements: failed to validate adjustment movement", zap.String("line_id", line.LineID), zap.Error(err))
			writeError(w, http.StatusUnprocessableEntity, "adjustment_validation_failed", err.Error())
			return
		}
		if !h.checkPeriodOpen(w, r, tenantID, sc.LegalEntityID, sc.FiscalPeriod) {
			return
		}
		if err := h.store.CommitMovement(r.Context(), m.MovementID, principalID, time.Now().UTC()); err != nil {
			h.log.Error("GenerateAdjustmentMovements: failed to commit adjustment movement", zap.String("line_id", line.LineID), zap.Error(err))
			writeError(w, http.StatusUnprocessableEntity, "adjustment_commit_failed", err.Error())
			return
		}
		if err := h.store.LinkCountLineAdjustment(r.Context(), line.LineID, m.MovementID); err != nil {
			h.log.Error("GenerateAdjustmentMovements: adjustment committed but could not be linked", zap.String("line_id", line.LineID), zap.String("movement_id", m.MovementID), zap.Error(err))
			writeError(w, http.StatusInternalServerError, "adjustment_not_linked",
				"the adjustment movement IS committed ("+m.MovementID+"), but the count line could not be linked to it.")
			return
		}
		h.publisher.PublishStockCountAdjustmentRequested(r.Context(), getCorrelationID(r), principalID, tenantID, line.LineID, m.MovementID)
		generated++
	}
	if generated == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_variance_approved_lines", domain.ErrNoVarianceApprovedLines.Error())
		return
	}
	if err := h.store.MarkCountAdjustmentsGenerated(r.Context(), id); err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count_id": id, "status": domain.StockCountStatusAdjustmentsGenerated, "adjustments_generated": generated})
}

// ── POST /v1/stock-counts/{id}/certify ────────────────────────────────────────

func (h *Handler) CertifyStockCount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	sc, err := h.store.GetStockCount(r.Context(), id)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountCertify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.CertifyStockCount(r.Context(), id, principalID, now); err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	sc.Status, sc.CertifiedAt, sc.CertifiedByPrincipalID = domain.StockCountStatusCertified, &now, &principalID
	h.publisher.PublishStockCountCertified(r.Context(), getCorrelationID(r), principalID, tenantID, *sc)
	writeJSON(w, http.StatusOK, sc)
}

// ── POST /v1/stock-counts/{id}/cancel ─────────────────────────────────────────

func (h *Handler) CancelStockCount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.CancelStockCountRequest
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
	sc, err := h.store.GetStockCount(r.Context(), id)
	if err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sc.LegalEntityID, actionInventoryCountPlan); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.CancelStockCount(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeStockCountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"count_id": id, "status": domain.StockCountStatusCancelled})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func strPtrLocal(s string) *string { return &s }

func (h *Handler) writeStockCountErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrStockCountNotFound):
		writeError(w, http.StatusNotFound, "stock_count_not_found", "")
	case errors.Is(err, domain.ErrCountLineNotFound):
		writeError(w, http.StatusNotFound, "count_line_not_found", "")
	case errors.Is(err, domain.ErrInvalidCountTransition), errors.Is(err, domain.ErrInvalidCountLineTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("stock count store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
