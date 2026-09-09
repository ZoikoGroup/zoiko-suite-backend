package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/valuation/movements/{movementID}/value ─────────────────────────

// ValueMovement lands directly in FINAL — see migration 000004's doc
// comment for why no separate Draft/Calculated/Validated command exists.
func (h *Handler) ValueMovement(w http.ResponseWriter, r *http.Request) {
	movementID := chi.URLParam(r, "movementID")
	var req domain.ValueMovementRequest
	if !decodeJSON(w, r, &req) {
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
	m, err := h.store.GetMovement(r.Context(), movementID)
	if err != nil {
		h.writeMovementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, m.LegalEntityID, actionInventoryValuationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	entry, err := h.store.ValueMovement(r.Context(), movementID, principalID, req.UnitCost, time.Now().UTC())
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	h.publisher.PublishInventoryValued(r.Context(), getCorrelationID(r), principalID, tenantID, *entry)
	writeJSON(w, http.StatusCreated, entry)
}

// ── GET /v1/valuation/entries/{id} ────────────────────────────────────────────

func (h *Handler) GetValuationEntry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetValuationEntry(r.Context(), id)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionInventoryValuationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ── GET /v1/valuation/inventory-value, /cost-layers ───────────────────────────

func (h *Handler) GetInventoryValue(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryValuationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	value, err := h.store.GetInventoryValue(r.Context(), itemID, locationID)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item_id": itemID, "location_id": locationID, "inventory_value": value})
}

func (h *Handler) GetCostLayers(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryValuationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	layers, err := h.store.GetCostLayers(r.Context(), itemID, locationID)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if layers == nil {
		layers = []domain.CostLayer{}
	}
	writeJSON(w, http.StatusOK, layers)
}

// ── POST /v1/valuation/runs ────────────────────────────────────────────────

func (h *Handler) CreateValuationRun(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateValuationRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || req.InventoryAccountCode == "" || req.COGSAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, fiscal_period, inventory_account_code and cogs_account_code are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionInventoryValuationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	run := &domain.ValuationRun{
		RunID: uuid.NewString(), LegalEntityID: req.LegalEntityID, FiscalPeriod: req.FiscalPeriod,
		InventoryAccountCode: req.InventoryAccountCode, COGSAccountCode: req.COGSAccountCode,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	frozenCount, err := h.store.CreateValuationRun(r.Context(), run)
	if err != nil {
		if errors.Is(err, domain.ErrValuationRunAlreadyExistsForPeriod) {
			writeError(w, http.StatusUnprocessableEntity, "run_already_exists_for_period", err.Error())
			return
		}
		h.log.Error("failed to create valuation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if frozenCount == 0 {
		writeError(w, http.StatusUnprocessableEntity, "empty_valuation_population", domain.ErrEmptyValuationPopulation.Error())
		return
	}
	run.Status = domain.ValuationRunStatusPopulationFrozen
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": run.RunID, "status": run.Status, "frozen_count": frozenCount})
}

// ── GET /v1/valuation/runs/{id} ────────────────────────────────────────────

func (h *Handler) GetValuationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetValuationRun(r.Context(), id)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionInventoryValuationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ── POST /v1/valuation/runs/{id}/emit ─────────────────────────────────────────

// EmitInventoryAccountingEvent collapses Calculated/Approved into this
// one command — see migration 000004's doc comment for why no separate
// approve command exists. Refuses self-approval: the emitting principal
// must differ from the run's own creator, the same maker/checker posture
// AST-02's own ApproveDepreciationRun applies.
//
// Posts only the OUTBOUND (COGS) side of this run's frozen population —
// debit COGS, credit Inventory — the one real, balanced entry INV-04 can
// make on its own. INBOUND entries (inventory value added by RECEIPT/
// ADJUSTMENT-increase movements) are deliberately NOT posted here: a
// balanced entry for them needs an offsetting AP/GRNI accrual account
// this platform does not model yet, and inventing an unbalanced entry
// would be worse than not posting at all — a real, stated gap, not a
// silent one (see the findings doc).
func (h *Handler) EmitInventoryAccountingEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetValuationRun(r.Context(), id)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if run.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermittedRun.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionInventoryValuationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("EmitInventoryAccountingEvent: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}

	var totalOutbound float64
	for _, e := range run.Entries {
		if e.EntryType == domain.ValuationEntryTypeOutbound {
			totalOutbound += e.Value
		}
	}
	if totalOutbound == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_valuation_to_emit", "this run's frozen population has no OUTBOUND (COGS) value to post")
		return
	}
	lines := []clients.LedgerLine{
		{AccountCode: run.COGSAccountCode, DebitAmount: totalOutbound},
		{AccountCode: run.InventoryAccountCode, CreditAmount: totalOutbound},
	}

	correlationID := getCorrelationID(r)
	journalID, err := h.ledger.PostInventoryAccountingEvent(r.Context(), tenantID, principalID, run.LegalEntityID, run.FiscalPeriod,
		"Inventory valuation run "+id, id, correlationID, lines)
	if err != nil {
		h.log.Error("EmitInventoryAccountingEvent: journal posting failed", zap.String("run_id", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	if err := h.store.MarkValuationRunEmitted(r.Context(), id, principalID, journalID, now); err != nil {
		h.log.Error("valuation run posted but could not be marked ACCOUNTING_EVENT_EMITTED",
			zap.String("run_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "run_not_recorded",
			"the journal IS posted ("+journalID+"), but the run could not be marked ACCOUNTING_EVENT_EMITTED.")
		return
	}
	h.publisher.PublishInventoryAccountingEventEmitted(r.Context(), correlationID, principalID, tenantID, run.LegalEntityID, id, journalID)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.ValuationRunStatusAccountingEventEmitted, "journal_id": journalID})
}

// ── POST /v1/valuation/write-downs ────────────────────────────────────────────

// RecordInventoryWriteDown refuses (negative path #3, "NRV write-down
// lacks evidence") without a recorded valuation_evidence_ref, and posts
// immediately — the spec's own SoD ("write-down/reversal requires
// evidence and approval where material") is satisfied by requiring the
// more senior actionInventoryValuationApprove action to create one at
// all, rather than a separate create-then-approve step no command in the
// spec actually names.
func (h *Handler) RecordInventoryWriteDown(w http.ResponseWriter, r *http.Request) {
	var req domain.RecordWriteDownRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ItemID == "" || req.LocationID == "" || req.ExpenseAccountCode == "" || req.InventoryAccountCode == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "item_id, location_id, expense_account_code, inventory_account_code and fiscal_period are required")
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
	item, err := h.store.GetItem(r.Context(), req.ItemID)
	if err != nil {
		h.writeItemErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, item.LegalEntityID, actionInventoryValuationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("RecordInventoryWriteDown: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if !h.checkPeriodOpen(w, r, tenantID, item.LegalEntityID, req.FiscalPeriod) {
		return
	}

	w2 := &domain.WriteDown{
		WriteDownID: uuid.NewString(), LegalEntityID: item.LegalEntityID, ItemID: req.ItemID, LocationID: req.LocationID,
		Amount: req.Amount, ValuationEvidenceRef: req.ValuationEvidenceRef,
		ExpenseAccountCode: req.ExpenseAccountCode, InventoryAccountCode: req.InventoryAccountCode,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	correlationID := getCorrelationID(r)
	lines := []clients.LedgerLine{
		{AccountCode: req.ExpenseAccountCode, DebitAmount: req.Amount},
		{AccountCode: req.InventoryAccountCode, CreditAmount: req.Amount},
	}
	journalID, err := h.ledger.PostInventoryAccountingEvent(r.Context(), tenantID, principalID, item.LegalEntityID, req.FiscalPeriod,
		"Inventory write-down "+w2.WriteDownID, w2.WriteDownID, correlationID, lines)
	if err != nil {
		h.log.Error("RecordInventoryWriteDown: journal posting failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}
	if err := h.store.CreateWriteDown(r.Context(), w2, journalID); err != nil {
		h.log.Error("write-down posted but could not be recorded", zap.String("write_down_id", w2.WriteDownID), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "write_down_not_recorded",
			"the journal IS posted ("+journalID+"), but the write-down could not be recorded.")
		return
	}
	w2.JournalID, w2.Status = &journalID, domain.WriteDownStatusAccountingEventEmitted
	h.publisher.PublishInventoryWriteDownRecorded(r.Context(), correlationID, principalID, tenantID, *w2)
	writeJSON(w, http.StatusCreated, w2)
}

// ── GET /v1/valuation/write-downs/{id} ────────────────────────────────────────

func (h *Handler) GetWriteDown(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	wd, err := h.store.GetWriteDown(r.Context(), id)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, wd.LegalEntityID, actionInventoryValuationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wd)
}

// ── POST /v1/valuation/write-downs/{id}/reverse ───────────────────────────────

// ReverseWriteDown refuses self-reversal — the same maker/checker
// posture as every other correction command this session.
func (h *Handler) ReverseWriteDown(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReverseWriteDownRequest
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
	wd, err := h.store.GetWriteDown(r.Context(), id)
	if err != nil {
		h.writeValuationErr(w, err)
		return
	}
	if wd.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_reversal_not_permitted", domain.ErrSelfWriteDownReversalNotPermitted.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, wd.LegalEntityID, actionInventoryValuationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("ReverseWriteDown: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if wd.JournalID != nil {
		if err := h.ledger.ReverseInventoryJournal(r.Context(), tenantID, principalID, *wd.JournalID, req.Reason); err != nil {
			h.log.Error("ReverseWriteDown: journal reversal failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_reversal_failed", err.Error())
			return
		}
	}
	now := time.Now().UTC()
	if err := h.store.ReverseWriteDown(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeValuationErr(w, err)
		return
	}
	wd.Status, wd.ReversedAt, wd.ReversedByPrincipalID, wd.ReversalReason = domain.WriteDownStatusReversed, &now, &principalID, &req.Reason
	h.publisher.PublishInventoryWriteDownReversed(r.Context(), getCorrelationID(r), principalID, tenantID, *wd)
	writeJSON(w, http.StatusOK, wd)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeValuationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrValuationEntryNotFound):
		writeError(w, http.StatusNotFound, "valuation_entry_not_found", "")
	case errors.Is(err, domain.ErrValuationRunNotFound):
		writeError(w, http.StatusNotFound, "valuation_run_not_found", "")
	case errors.Is(err, domain.ErrWriteDownNotFound):
		writeError(w, http.StatusNotFound, "write_down_not_found", "")
	case errors.Is(err, domain.ErrMovementAlreadyValued),
		errors.Is(err, domain.ErrMovementNotCommitted),
		errors.Is(err, domain.ErrUnitCostRequired),
		errors.Is(err, domain.ErrInsufficientCostLayers),
		errors.Is(err, domain.ErrValuationPolicyRequiredForActivation):
		writeError(w, http.StatusUnprocessableEntity, "valuation_failed", err.Error())
	case errors.Is(err, domain.ErrInvalidRunTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrWriteDownAlreadyReversed):
		writeError(w, http.StatusUnprocessableEntity, "already_reversed", err.Error())
	default:
		h.log.Error("inventory valuation store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
