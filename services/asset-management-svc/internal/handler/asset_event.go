package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/clients"
	"zoiko.io/asset-management-svc/internal/domain"
)

// ── POST /v1/asset-events ────────────────────────────────────────────────────

// CreateAssetEvent is the one real create path — see migration 000003's
// doc comment. RecordDisposal/RecordImpairment/RecordRevaluation/
// RecordComponentReplacement are thin wrappers that pre-set EventType and
// call the same internal logic; there is no separate implementation to
// keep in sync.
func (h *Handler) CreateAssetEvent(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAssetEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	h.createAssetEvent(w, r, req)
}

func (h *Handler) createAssetEvent(w http.ResponseWriter, r *http.Request, req domain.CreateAssetEventRequest) {
	if req.AssetID == "" || req.SourceDocumentRef == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "asset_id, source_document_ref and fiscal_period are required")
		return
	}
	if req.EventType == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrAssetEventTypeRequired.Error())
		return
	}
	switch req.EventType {
	case domain.AssetEventTypeCapitalization, domain.AssetEventTypeAddition, domain.AssetEventTypeComponentReplacement,
		domain.AssetEventTypeTransfer, domain.AssetEventTypeImpairment, domain.AssetEventTypeRevaluation, domain.AssetEventTypeDisposal:
	default:
		writeError(w, http.StatusBadRequest, "invalid_event_type", "event_type must be one of CAPITALIZATION, ADDITION, COMPONENT_REPLACEMENT, TRANSFER, IMPAIRMENT, REVALUATION, DISPOSAL")
		return
	}
	if req.EventType == domain.AssetEventTypeComponentReplacement && req.ComponentID == "" {
		writeError(w, http.StatusUnprocessableEntity, "component_required", domain.ErrComponentRequiredForReplacement.Error())
		return
	}
	if req.EventType == domain.AssetEventTypeTransfer && req.DestinationLegalEntityID != "" {
		// Negative path #3, "Cross-entity asset transfer bypasses
		// intercompany accounting" — refused here before the store is
		// even asked (the database's own chk_asset_event_no_cross_entity_transfer
		// re-verifies it as the authoritative check).
		asset, err := h.store.GetAsset(r.Context(), req.AssetID)
		if err != nil {
			h.writeAssetErr(w, err)
			return
		}
		if req.DestinationLegalEntityID != asset.LegalEntityID {
			writeError(w, http.StatusUnprocessableEntity, "cross_entity_transfer_blocked", domain.ErrCrossEntityTransferBlocked.Error())
			return
		}
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	asset, err := h.store.GetAsset(r.Context(), req.AssetID)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if req.EventType == domain.AssetEventTypeComponentReplacement {
		found := false
		for _, c := range asset.Components {
			if c.ComponentID == req.ComponentID {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusUnprocessableEntity, "component_not_on_asset", domain.ErrComponentNotOnAsset.Error())
			return
		}
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, asset.LegalEntityID, actionAssetEventCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	effectiveDate := time.Now().UTC()
	if req.EffectiveDate != nil {
		effectiveDate = *req.EffectiveDate
	}
	e := &domain.AssetEvent{
		EventID: uuid.NewString(), LegalEntityID: asset.LegalEntityID, AssetID: req.AssetID,
		EventType: req.EventType, Status: domain.AssetEventStatusDraft,
		SourceDocumentRef: req.SourceDocumentRef, Amount: req.Amount, EffectiveDate: effectiveDate,
		FiscalPeriod: req.FiscalPeriod, ProceedsAmount: req.ProceedsAmount,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if req.ComponentID != "" {
		e.ComponentID = &req.ComponentID
	}
	if req.ValuationEvidenceRef != "" {
		e.ValuationEvidenceRef = &req.ValuationEvidenceRef
	}
	if req.Currency != "" {
		e.Currency = &req.Currency
	}
	if req.DestinationCustodianID != "" {
		e.DestinationCustodianID = &req.DestinationCustodianID
	}
	if req.DestinationLocationID != "" {
		e.DestinationLocationID = &req.DestinationLocationID
	}
	if req.DestinationLegalEntityID != "" {
		e.DestinationLegalEntityID = &req.DestinationLegalEntityID
	}
	if req.DebitAccountCode != "" {
		e.DebitAccountCode = &req.DebitAccountCode
	}
	if req.CreditAccountCode != "" {
		e.CreditAccountCode = &req.CreditAccountCode
	}
	if req.CorrectionOfEventID != "" {
		e.CorrectionOfEventID = &req.CorrectionOfEventID
	}

	if err := h.store.CreateAssetEvent(r.Context(), e); err != nil {
		h.log.Error("failed to create asset event", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// ── POST /v1/asset-events/record-disposal, record-impairment, ─────────────
// ── record-revaluation, record-component-replacement ───────────────────────
//
// Type-specific convenience entry points the spec names as their own
// explicit commands — each pre-sets EventType and additionally enforces
// that type's own required evidence before funneling into the shared
// createAssetEvent path.

func (h *Handler) RecordDisposal(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAssetEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.EventType = domain.AssetEventTypeDisposal
	if req.ProceedsAmount == nil {
		writeError(w, http.StatusUnprocessableEntity, "proceeds_required", domain.ErrProceedsRequiredForDisposal.Error())
		return
	}
	h.createAssetEvent(w, r, req)
}

func (h *Handler) RecordImpairment(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAssetEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.EventType = domain.AssetEventTypeImpairment
	h.createAssetEvent(w, r, req)
}

func (h *Handler) RecordRevaluation(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAssetEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.EventType = domain.AssetEventTypeRevaluation
	h.createAssetEvent(w, r, req)
}

func (h *Handler) RecordComponentReplacement(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAssetEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.EventType = domain.AssetEventTypeComponentReplacement
	h.createAssetEvent(w, r, req)
}

// ── GET /v1/asset-events/{id}, GET /v1/asset-events?asset_id= ───────────────

func (h *Handler) GetAssetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetAssetEvent(r.Context(), id)
	if err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionAssetEventView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ListAssetEvents backs the spec's own GetAssetEventChain/ExplainAssetState
// queries — every event recorded against one asset, most recent first.
func (h *Handler) ListAssetEvents(w http.ResponseWriter, r *http.Request) {
	assetID := r.URL.Query().Get("asset_id")
	if assetID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "asset_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	asset, err := h.store.GetAsset(r.Context(), assetID)
	if err != nil {
		h.writeAssetErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, asset.LegalEntityID, actionAssetEventView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListAssetEvents(r.Context(), assetID)
	if err != nil {
		h.log.Error("ListAssetEvents: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.AssetEvent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/asset-events/{id}/validate ──────────────────────────────────────

// ValidateAssetEvent enforces negative path #1, "Impairment amount entered
// without evidence/approval" — an IMPAIRMENT or REVALUATION event refuses
// to leave DRAFT without a recorded valuation_evidence_ref.
func (h *Handler) ValidateAssetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetAssetEvent(r.Context(), id)
	if err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	if (e.EventType == domain.AssetEventTypeImpairment || e.EventType == domain.AssetEventTypeRevaluation) &&
		(e.ValuationEvidenceRef == nil || *e.ValuationEvidenceRef == "") {
		writeError(w, http.StatusUnprocessableEntity, "valuation_evidence_required", domain.ErrValuationEvidenceRequired.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionAssetEventCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.ValidateAssetEvent(r.Context(), id, time.Now().UTC()); err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"event_id": id, "status": domain.AssetEventStatusValidated})
}

// ── POST /v1/asset-events/{id}/approve ───────────────────────────────────────

// ApproveAssetEvent refuses self-approval only for the three event types
// the spec itself names as material — IMPAIRMENT, REVALUATION, DISPOSAL.
// The other four carry no approval-time $ consequence in this v1, so
// same-principal approval is permitted for them.
func (h *Handler) ApproveAssetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetAssetEvent(r.Context(), id)
	if err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	if domain.IsMaterialAssetEventType(e.EventType) && e.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermittedEvent.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionAssetEventApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveAssetEvent(r.Context(), id, principalID, now); err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"event_id": id, "status": domain.AssetEventStatusApproved})
}

// ── POST /v1/asset-events/{id}/apply ─────────────────────────────────────────

// ApplyAssetEvent performs the real book-state delta (DISPOSAL only, in
// this v1) and, where the event carries a $ amount, the ledger posting in
// the same command — see migration 000003's doc comment for why no
// separate Emit command exists. Enforces negative path #4, "Hard-closed-
// period event silently backdated," via financial-close-svc's real
// period-status check before anything is written.
func (h *Handler) ApplyAssetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	e, err := h.store.GetAssetEvent(r.Context(), id)
	if err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionAssetEventApply); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("ApplyAssetEvent: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if err := h.ledger.CheckPeriodOpen(r.Context(), tenantID, e.LegalEntityID, e.FiscalPeriod); err != nil {
		if errors.Is(err, domain.ErrPeriodLocked) {
			writeError(w, http.StatusUnprocessableEntity, "period_locked", err.Error())
			return
		}
		h.log.Error("ApplyAssetEvent: period check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "period_check_unavailable", err.Error())
		return
	}

	var journalID *string
	if e.Amount != nil && e.DebitAccountCode != nil && e.CreditAccountCode != nil {
		correlationID := getCorrelationID(r)
		lines := []clients.LedgerLine{
			{AccountCode: *e.DebitAccountCode, DebitAmount: *e.Amount},
			{AccountCode: *e.CreditAccountCode, CreditAmount: *e.Amount},
		}
		postedJournalID, err := h.ledger.PostAssetEventAccountingEvent(r.Context(), tenantID, principalID, e.LegalEntityID, e.FiscalPeriod,
			"Asset event "+e.EventID+" ("+e.EventType+")", e.EventID, correlationID, lines)
		if err != nil {
			h.log.Error("ApplyAssetEvent: journal posting failed", zap.String("event_id", e.EventID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
			return
		}
		journalID = &postedJournalID
	}

	now := time.Now().UTC()
	if err := h.store.ApplyAssetEvent(r.Context(), id, now, journalID); err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	status := domain.AssetEventStatusApplied
	if journalID != nil {
		status = domain.AssetEventStatusAccountingEventEmitted
	}
	resp := map[string]any{"event_id": id, "status": status}
	if journalID != nil {
		resp["journal_id"] = *journalID
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── POST /v1/asset-events/{id}/reverse, /supersede ───────────────────────────

// ReverseAssetEvent implements negative path #2's own correction side —
// "applied events corrected only by reverse/supersede" — never an
// in-place edit (also structurally impossible: migration 000003's own
// reject-mutation trigger).
func (h *Handler) ReverseAssetEvent(w http.ResponseWriter, r *http.Request) {
	h.correctAssetEvent(w, r, false)
}

func (h *Handler) SupersedeAssetEvent(w http.ResponseWriter, r *http.Request) {
	h.correctAssetEvent(w, r, true)
}

func (h *Handler) correctAssetEvent(w http.ResponseWriter, r *http.Request, supersede bool) {
	id := chi.URLParam(r, "id")
	var req domain.ReverseAssetEventRequest
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
	e, err := h.store.GetAssetEvent(r.Context(), id)
	if err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionAssetEventCorrect); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("correctAssetEvent: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if e.JournalID != nil {
		if err := h.ledger.ReverseAssetEventJournal(r.Context(), tenantID, principalID, *e.JournalID, req.Reason); err != nil {
			h.log.Error("correctAssetEvent: journal reversal failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_reversal_failed", err.Error())
			return
		}
	}
	now := time.Now().UTC()
	status := domain.AssetEventStatusReversed
	if supersede {
		status = domain.AssetEventStatusSuperseded
		err = h.store.SupersedeAssetEvent(r.Context(), id, principalID, req.Reason, now)
	} else {
		err = h.store.ReverseAssetEvent(r.Context(), id, principalID, req.Reason, now)
	}
	if err != nil {
		h.writeAssetEventErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"event_id": id, "status": status})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeAssetEventErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAssetEventNotFound):
		writeError(w, http.StatusNotFound, "asset_event_not_found", "")
	case errors.Is(err, domain.ErrInvalidAssetEventTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrAssetNotEligibleForDisposal):
		writeError(w, http.StatusUnprocessableEntity, "asset_not_eligible_for_disposal", err.Error())
	default:
		h.log.Error("asset event store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
