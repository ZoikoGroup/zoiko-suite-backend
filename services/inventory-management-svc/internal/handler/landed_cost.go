package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/valuation/landed-costs ──────────────────────────────────────────

// AllocateLandedCost is INV-04's AllocateLandedCost command (spec §10 "Late-
// arriving cost"): a late freight/landed cost is applied to the cost layer of
// a valued RECEIPT as a controlled revaluation and posted in the current open
// period — never by rewriting the receipt's original period. See migration
// 000009 for the model (inventory share / COGS true-up / one journal).
//
// Requires evidence (spec: costs are source-linked and policy-classified) and
// the senior valuation-approve action (same posture as write-downs). It is
// idempotent on idempotency_key, so a posting that failed after the layer was
// updated is resumed by retrying the same request — the cost is never applied
// twice.
func (h *Handler) AllocateLandedCost(w http.ResponseWriter, r *http.Request) {
	var req domain.AllocateLandedCostRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.MovementID == "" || req.IdempotencyKey == "" || req.FiscalPeriod == "" ||
		req.InventoryAccountCode == "" || req.COGSAccountCode == "" || req.OffsetAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"movement_id, idempotency_key, fiscal_period, inventory_account_code, cogs_account_code and offset_account_code are required")
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount must be positive")
		return
	}
	if req.ValuationEvidenceRef == "" {
		writeError(w, http.StatusUnprocessableEntity, "valuation_evidence_required", domain.ErrValuationEvidenceRequired.Error())
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
	mv, err := h.store.GetMovement(r.Context(), req.MovementID)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, mv.LegalEntityID, actionInventoryValuationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("AllocateLandedCost: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	// The correction posts in the period named here; a locked period refuses.
	if !h.checkPeriodOpen(w, r, tenantID, mv.LegalEntityID, req.FiscalPeriod) {
		return
	}

	a := &domain.LandedCostAllocation{
		AllocationID: uuid.NewString(), MovementID: req.MovementID, IdempotencyKey: req.IdempotencyKey,
		Amount: req.Amount, ValuationEvidenceRef: req.ValuationEvidenceRef, FiscalPeriod: req.FiscalPeriod,
		InventoryAccountCode: req.InventoryAccountCode, COGSAccountCode: req.COGSAccountCode, OffsetAccountCode: req.OffsetAccountCode,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	created, err := h.store.AllocateLandedCost(r.Context(), a)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}

	if a.Status == domain.LandedCostStatusPendingPosting {
		var lines []clients.LedgerLine
		if a.InventoryShare > 0 {
			lines = append(lines, clients.LedgerLine{AccountCode: a.InventoryAccountCode, DebitAmount: a.InventoryShare})
		}
		if a.COGSShare > 0 {
			lines = append(lines, clients.LedgerLine{AccountCode: a.COGSAccountCode, DebitAmount: a.COGSShare})
		}
		lines = append(lines, clients.LedgerLine{AccountCode: a.OffsetAccountCode, CreditAmount: a.Amount})

		correlationID := getCorrelationID(r)
		journalID, err := h.ledger.PostInventoryAccountingEvent(r.Context(), tenantID, principalID, a.LegalEntityID, a.FiscalPeriod,
			"Inventory landed cost "+a.AllocationID, a.AllocationID, correlationID, lines)
		if err != nil {
			// The layer is already updated and the allocation is PENDING_POSTING:
			// retrying the same idempotency_key resumes the posting.
			h.log.Error("AllocateLandedCost: journal posting failed", zap.String("allocation_id", a.AllocationID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_posting_failed",
				"landed cost recorded as "+a.AllocationID+" but not yet posted; retry the same idempotency_key to resume")
			return
		}
		now := time.Now().UTC()
		if err := h.store.MarkLandedCostEmitted(r.Context(), a.AllocationID, journalID, now); err != nil {
			h.log.Error("landed cost posted but could not be marked emitted",
				zap.String("allocation_id", a.AllocationID), zap.String("journal_id", journalID), zap.Error(err))
			writeError(w, http.StatusInternalServerError, "landed_cost_not_recorded",
				"the journal IS posted ("+journalID+"), but the allocation could not be marked emitted; retry the same idempotency_key")
			return
		}
		a.Status, a.JournalID, a.EmittedAt = domain.LandedCostStatusAccountingEventEmitted, &journalID, &now
		h.publisher.PublishInventoryAccountingEventEmitted(r.Context(), correlationID, principalID, tenantID, a.LegalEntityID, a.AllocationID, journalID)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, a)
}

// ── GET /v1/valuation/landed-costs/{id} ──────────────────────────────────────

func (h *Handler) GetLandedCostAllocation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	a, err := h.store.GetLandedCostAllocation(r.Context(), id)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.LegalEntityID, actionInventoryValuationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
