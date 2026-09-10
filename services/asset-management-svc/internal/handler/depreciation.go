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

// ── POST /v1/depreciation-schedules ──────────────────────────────────────────

// BuildDepreciationSchedule lands a schedule directly in ACTIVE — see
// migration 000002's doc comment for why no command reaches Draft/Validated
// independently. Refuses (422) against an asset that isn't ACTIVE
// (capitalized) — depreciation requires a real, capitalized asset to
// depreciate.
func (h *Handler) BuildDepreciationSchedule(w http.ResponseWriter, r *http.Request) {
	var req domain.BuildDepreciationScheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AssetID == "" || req.BookID == "" || req.UsefulLifeMonths <= 0 || req.CostBasis <= 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "asset_id, book_id, cost_basis and useful_life_months (>0) are required")
		return
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
	if asset.Status != domain.AssetStatusActive {
		writeError(w, http.StatusUnprocessableEntity, "asset_not_eligible", domain.ErrAssetNotEligibleForSchedule.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, asset.LegalEntityID, actionDepreciationScheduleBuild); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	inServiceDate := time.Now().UTC()
	if req.InServiceDate != nil {
		inServiceDate = *req.InServiceDate
	}
	scheduleID := uuid.NewString()
	sch := &domain.DepreciationSchedule{
		ScheduleVersionID: uuid.NewString(), ScheduleID: scheduleID, Version: 1,
		LegalEntityID: asset.LegalEntityID, AssetID: req.AssetID, BookID: req.BookID,
		Method: domain.DepreciationMethodStraightLine, CostBasis: req.CostBasis, ResidualValue: req.ResidualValue,
		UsefulLifeMonths: req.UsefulLifeMonths, InServiceDate: inServiceDate,
		Status: domain.DepreciationScheduleStatusActive, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateDepreciationSchedule(r.Context(), sch); err != nil {
		if errors.Is(err, domain.ErrDuplicateScheduleForAssetBook) {
			writeError(w, http.StatusUnprocessableEntity, "duplicate_schedule_for_asset_book", err.Error())
			return
		}
		h.log.Error("failed to build depreciation schedule", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

// ── GET /v1/depreciation-schedules/completeness ───────────────────────────────

// GetDepreciationCompleteness is a read-only coverage check — never a
// caller-declared figure. financial-close-svc's ACC-06 calls this
// directly for the AST/INV/PRJ domain spec's own §9 "Depreciation
// completeness" assertion; see internal/store's own doc comment for the
// exact eligible-vs-covered calculation.
func (h *Handler) GetDepreciationCompleteness(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionDepreciationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	covered, eligible, err := h.store.GetDepreciationCompleteness(r.Context(), legalEntityID, fiscalPeriod)
	if err != nil {
		h.log.Error("GetDepreciationCompleteness: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"covered_count": covered, "eligible_count": eligible})
}

// ── GET /v1/depreciation-schedules/{id} ──────────────────────────────────────

func (h *Handler) GetDepreciationSchedule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetCurrentDepreciationSchedule(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionDepreciationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

// ── POST /v1/depreciation-schedules/{id}/recalculate ─────────────────────────

// RecalculateSchedule is the ONLY way a schedule's own parameters change
// — the spec's own negative path, "Useful life changed after approval
// without invalidation," has no in-place edit path: the current version
// is end-dated and a new one takes its place.
func (h *Handler) RecalculateSchedule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RecalculateScheduleRequest
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
	current, err := h.store.GetCurrentDepreciationSchedule(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionDepreciationScheduleBuild); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	newVersion := &domain.DepreciationSchedule{
		ScheduleVersionID: uuid.NewString(), LegalEntityID: current.LegalEntityID,
		AssetID: current.AssetID, BookID: current.BookID, Method: current.Method,
		CostBasis: current.CostBasis, ResidualValue: current.ResidualValue, UsefulLifeMonths: current.UsefulLifeMonths,
		InServiceDate: current.InServiceDate, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if req.CostBasis != nil {
		newVersion.CostBasis = *req.CostBasis
	}
	if req.ResidualValue != nil {
		newVersion.ResidualValue = *req.ResidualValue
	}
	if req.UsefulLifeMonths != nil {
		newVersion.UsefulLifeMonths = *req.UsefulLifeMonths
	}

	if err := h.store.RecalculateSchedule(r.Context(), id, newVersion, time.Now().UTC()); err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	newVersion.ScheduleID = id
	newVersion.Status = domain.DepreciationScheduleStatusActive
	writeJSON(w, http.StatusCreated, newVersion)
}

// ── POST /v1/depreciation-runs ────────────────────────────────────────────────

func (h *Handler) CreateDepreciationRun(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDepreciationRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || req.DepreciationExpenseAccountCode == "" || req.AccumulatedDepreciationAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, fiscal_period, depreciation_expense_account_code and accumulated_depreciation_account_code are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionDepreciationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	run := &domain.DepreciationRun{
		RunID: uuid.NewString(), LegalEntityID: req.LegalEntityID, FiscalPeriod: req.FiscalPeriod,
		DepreciationExpenseAccountCode: req.DepreciationExpenseAccountCode, AccumulatedDepreciationAccountCode: req.AccumulatedDepreciationAccountCode,
		Status: domain.DepreciationRunStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateDepreciationRun(r.Context(), run); err != nil {
		if errors.Is(err, domain.ErrRunAlreadyExistsForPeriod) {
			writeError(w, http.StatusUnprocessableEntity, "run_already_exists_for_period", err.Error())
			return
		}
		h.log.Error("failed to create depreciation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// ── GET /v1/depreciation-runs/{id} ───────────────────────────────────────────

func (h *Handler) GetDepreciationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetDepreciationRun(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionDepreciationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ── POST /v1/depreciation-runs/{id}/freeze ────────────────────────────────────

// FreezeDepreciationPopulation is the spec's own named evidence step —
// "frozen population manifest" — locking in exactly which current, ACTIVE
// schedules belong to this run before any calculation happens.
func (h *Handler) FreezeDepreciationPopulation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetDepreciationRun(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionDepreciationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	frozenCount, err := h.store.FreezeDepreciationPopulation(r.Context(), id, run.LegalEntityID, time.Now().UTC())
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if frozenCount == 0 {
		writeError(w, http.StatusUnprocessableEntity, "empty_frozen_population", domain.ErrEmptyFrozenPopulation.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.DepreciationRunStatusPopulationFrozen, "frozen_count": frozenCount})
}

// ── POST /v1/depreciation-runs/{id}/validate ─────────────────────────────────

// ValidateDepreciationRun performs the actual calculation AND validates it
// in one step — see migration 000002's doc comment for why Run's own
// Calculated state has no dedicated command.
func (h *Handler) ValidateDepreciationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetDepreciationRun(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionDepreciationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	lineCount, err := h.store.ValidateDepreciationRun(r.Context(), id, time.Now().UTC())
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.DepreciationRunStatusValidated, "line_count": lineCount})
}

// ── POST /v1/depreciation-runs/{id}/approve ───────────────────────────────────

// ApproveDepreciationRun refuses self-approval — the spec's own SoD:
// "Run preparer cannot self-approve material exceptions." No severity
// concept exists in this v1, so — the same bootstrap-gap direction ACC-03
// took for maker/checker — self-approval is refused universally, not
// selectively.
func (h *Handler) ApproveDepreciationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetDepreciationRun(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if run.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermittedRun.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionDepreciationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveDepreciationRun(r.Context(), id, principalID, now); err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run_id": id, "status": domain.DepreciationRunStatusApproved})
}

// ── POST /v1/depreciation-runs/{id}/emit ──────────────────────────────────────

// EmitDepreciationAccountingEvent posts one multi-line journal — debit
// depreciation expense, credit accumulated depreciation, one pair of
// lines per depreciation_line — through general-ledger-svc's real ACC-04
// posting path, keyed by this run's own ID for idempotency. The spec's
// own negative path, "Rerun emits duplicate accounting event," is
// enforced by GL's own UNIQUE(tenant_id, source_event_id), not
// reimplemented here.
func (h *Handler) EmitDepreciationAccountingEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetDepreciationRun(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionDepreciationRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("EmitDepreciationAccountingEvent: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}

	var lines []clients.LedgerLine
	var total float64
	for _, l := range run.Lines {
		if l.PeriodAmount == 0 {
			continue
		}
		lines = append(lines,
			clients.LedgerLine{AccountCode: run.DepreciationExpenseAccountCode, DebitAmount: l.PeriodAmount},
			clients.LedgerLine{AccountCode: run.AccumulatedDepreciationAccountCode, CreditAmount: l.PeriodAmount},
		)
		total += l.PeriodAmount
	}
	if len(lines) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_depreciation_to_emit", "every line in this run's frozen population depreciated to zero")
		return
	}

	correlationID := getCorrelationID(r)
	journalID, err := h.ledger.PostDepreciationAccountingEvent(r.Context(), tenantID, principalID, run.LegalEntityID, run.FiscalPeriod,
		"Depreciation run "+id, id, correlationID, lines)
	if err != nil {
		h.log.Error("EmitDepreciationAccountingEvent: journal posting failed", zap.String("run_id", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	if err := h.store.MarkDepreciationRunEmitted(r.Context(), id, journalID, now); err != nil {
		h.log.Error("depreciation run posted but could not be marked ACCOUNTING_EVENT_EMITTED",
			zap.String("run_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "run_not_recorded",
			"the journal IS posted ("+journalID+"), but the run could not be marked ACCOUNTING_EVENT_EMITTED.")
		return
	}
	h.publisher.PublishDepreciationRunAccountingEventEmitted(r.Context(), correlationID, principalID, tenantID, run.LegalEntityID, id, journalID)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.DepreciationRunStatusAccountingEventEmitted, "journal_id": journalID})
}

// ── POST /v1/depreciation-runs/{id}/supersede ─────────────────────────────────

// SupersedeDepreciationRun reverses this run's already-emitted journal and
// marks the run SUPERSEDED — see migration 000002's doc comment for why
// this is the real interpretation of a command named for the Run even
// though the spec's own Run state model never lists "Superseded."
// Releases the (entity, period) slot so a fresh run can be created.
func (h *Handler) SupersedeDepreciationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SupersedeDepreciationRunRequest
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
	run, err := h.store.GetDepreciationRun(r.Context(), id)
	if err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionDepreciationSupersede); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("SupersedeDepreciationRun: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if run.JournalID != nil {
		if err := h.ledger.ReverseDepreciationJournal(r.Context(), tenantID, principalID, *run.JournalID, req.Reason); err != nil {
			h.log.Error("SupersedeDepreciationRun: journal reversal failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_reversal_failed", err.Error())
			return
		}
	}
	now := time.Now().UTC()
	if err := h.store.SupersedeDepreciationRun(r.Context(), id, principalID, now); err != nil {
		h.writeDepreciationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run_id": id, "status": domain.DepreciationRunStatusSuperseded})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeDepreciationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrScheduleNotFound):
		writeError(w, http.StatusNotFound, "schedule_not_found", "")
	case errors.Is(err, domain.ErrRunNotFound):
		writeError(w, http.StatusNotFound, "run_not_found", "")
	case errors.Is(err, domain.ErrInvalidRunTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("depreciation store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
