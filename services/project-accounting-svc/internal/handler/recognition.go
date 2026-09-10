package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/clients"
	"zoiko.io/project-accounting-svc/internal/domain"
)

// ── POST/GET /v1/recognition/estimates ────────────────────────────────────────

// SetApprovedEstimate refuses any effective_from that isn't strictly
// later than the moment this command runs — see migration 000003's doc
// comment on negative path #2. Mirrors INV-01's own
// SetValuationPolicyFutureEffective exactly.
func (h *Handler) SetApprovedEstimate(w http.ResponseWriter, r *http.Request) {
	var req domain.SetApprovedEstimateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	now := time.Now().UTC()
	effectiveFrom := now
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	if !effectiveFrom.After(now) {
		writeError(w, http.StatusUnprocessableEntity, "not_future_effective", domain.ErrEstimateMustBeFutureEffective.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), req.ProjectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectEstimateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	newVersion := &domain.RecognitionEstimate{
		EstimateVersionID: uuid.NewString(), ProjectID: req.ProjectID, EstimateToComplete: req.EstimateToComplete,
		EffectiveFrom: effectiveFrom, CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	if err := h.store.SetApprovedEstimate(r.Context(), newVersion); err != nil {
		h.log.Error("failed to set approved estimate", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, newVersion)
}

// GetPostedRevenueTotal is a read-only aggregate over real, live
// recognition runs — never a caller-declared figure. financial-close-svc's
// ACC-06 calls this directly for the AST/INV/PRJ domain spec's own §9
// "Project revenue/WIP → GL" assertion; see internal/store's own doc
// comment for the exact calculation.
func (h *Handler) GetPostedRevenueTotal(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionProjectRevenueRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	total, err := h.store.GetPostedRevenueTotal(r.Context(), legalEntityID, fiscalPeriod)
	if err != nil {
		h.log.Error("GetPostedRevenueTotal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]float64{"posted_revenue_total": total})
}

func (h *Handler) GetCurrentEstimate(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRevenueRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	est, err := h.store.GetCurrentEstimate(r.Context(), projectID)
	if err != nil {
		h.log.Error("GetCurrentEstimate: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if est == nil {
		writeError(w, http.StatusNotFound, "estimate_not_set", "")
		return
	}
	writeJSON(w, http.StatusOK, est)
}

// ── POST /v1/recognition/runs ──────────────────────────────────────────────

func (h *Handler) CreateRecognitionRun(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRecognitionRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id and fiscal_period are required")
		return
	}
	if req.ContractValue <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "contract_value_required", domain.ErrContractValueRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), req.ProjectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRevenueRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	run := &domain.RecognitionRun{
		RunID: uuid.NewString(), LegalEntityID: p.LegalEntityID, ProjectID: req.ProjectID, FiscalPeriod: req.FiscalPeriod,
		Status: domain.RecognitionRunStatusDraft, ContractValue: &req.ContractValue, BilledToDate: &req.BilledToDate,
		RevenueAccountCode: req.RevenueAccountCode, WIPAccountCode: req.WIPAccountCode,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateRecognitionRun(r.Context(), run); err != nil {
		if errors.Is(err, domain.ErrRecognitionRunAlreadyExistsForPeriod) {
			writeError(w, http.StatusUnprocessableEntity, "run_already_exists_for_period", err.Error())
			return
		}
		h.log.Error("failed to create recognition run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// ── GET /v1/recognition/runs/{id} ────────────────────────────────────────────

func (h *Handler) GetRecognitionRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ── POST /v1/recognition/runs/{id}/calculate ─────────────────────────────────

// CalculateRecognitionRun is CalculateProjectRevenue + CalculateProjectWIP
// collapsed into one real step — see migration 000003's doc comment.
func (h *Handler) CalculateRecognitionRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.FreezeAndCalculate(r.Context(), id, time.Now().UTC()); err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	updated, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	tenantID, _ := h.requireTenant(w, r)
	h.publisher.PublishProjectRecognitionCalculated(r.Context(), getCorrelationID(r), principalID, tenantID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/recognition/runs/{id}/validate ──────────────────────────────────

func (h *Handler) ValidateRecognitionRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.ValidateRecognitionRun(r.Context(), id, time.Now().UTC()); err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run_id": id, "status": domain.RecognitionRunStatusReviewed})
}

// ── POST /v1/recognition/runs/{id}/approve ───────────────────────────────────

// ApproveRecognitionRun refuses self-approval — the spec's own SoD:
// "Estimator/preparer cannot self-approve material estimate or
// recognition override."
func (h *Handler) ApproveRecognitionRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if run.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermittedRecognition.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveRecognitionRun(r.Context(), id, principalID, now); err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	run.Status, run.ApprovedAt, run.ApprovedByPrincipalID = domain.RecognitionRunStatusApproved, &now, &principalID
	tenantID, _ := h.requireTenant(w, r)
	h.publisher.PublishProjectRevenueApproved(r.Context(), getCorrelationID(r), principalID, tenantID, *run)
	writeJSON(w, http.StatusOK, run)
}

// ── POST /v1/recognition/runs/{id}/emit ──────────────────────────────────────

// EmitRecognitionAccountingEvent enforces negative path #4, "closed
// period ... blocks certification," via financial-close-svc's real
// period-status check before the store is even asked.
func (h *Handler) EmitRecognitionAccountingEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("EmitRecognitionAccountingEvent: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if !h.checkPeriodOpen(w, r, tenantID, run.LegalEntityID, run.FiscalPeriod) {
		return
	}
	if run.PeriodRecognizedRevenue == nil || *run.PeriodRecognizedRevenue == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_revenue_to_emit", "this run's own period_recognized_revenue is zero")
		return
	}
	if run.RevenueAccountCode == "" || run.WIPAccountCode == "" {
		writeError(w, http.StatusUnprocessableEntity, "account_codes_required", "this run has no revenue_account_code/wip_account_code configured")
		return
	}

	revenue := *run.PeriodRecognizedRevenue
	var lines []clients.LedgerLine
	if revenue > 0 {
		lines = []clients.LedgerLine{
			{AccountCode: run.WIPAccountCode, DebitAmount: revenue},
			{AccountCode: run.RevenueAccountCode, CreditAmount: revenue},
		}
	} else {
		lines = []clients.LedgerLine{
			{AccountCode: run.RevenueAccountCode, DebitAmount: -revenue},
			{AccountCode: run.WIPAccountCode, CreditAmount: -revenue},
		}
	}

	correlationID := getCorrelationID(r)
	journalID, err := h.ledger.PostRecognitionAccountingEvent(r.Context(), tenantID, principalID, run.LegalEntityID, run.FiscalPeriod,
		"Project recognition run "+id, id, correlationID, lines)
	if err != nil {
		h.log.Error("EmitRecognitionAccountingEvent: journal posting failed", zap.String("run_id", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	if err := h.store.MarkRecognitionRunEmitted(r.Context(), id, journalID, now); err != nil {
		h.log.Error("recognition run posted but could not be marked ACCOUNTING_EVENT_EMITTED",
			zap.String("run_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "run_not_recorded",
			"the journal IS posted ("+journalID+"), but the run could not be marked ACCOUNTING_EVENT_EMITTED.")
		return
	}
	h.publisher.PublishProjectRecognitionAccountingEventEmitted(r.Context(), correlationID, principalID, tenantID, run.LegalEntityID, id, journalID)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "status": domain.RecognitionRunStatusAccountingEventEmitted, "journal_id": journalID})
}

// ── POST /v1/recognition/runs/{id}/supersede ─────────────────────────────────

// SupersedeRecognitionRun reverses this run's already-emitted journal and
// marks the run SUPERSEDED — mirrors AST-02's own SupersedeDepreciationRun
// exactly. Releases the (project, fiscal_period) slot so a fresh run can
// be created.
func (h *Handler) SupersedeRecognitionRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SupersedeRecognitionRunRequest
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
	run, err := h.store.GetRecognitionRun(r.Context(), id)
	if err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionProjectRevenueSupersede); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if h.ledger == nil {
		h.log.Error("SupersedeRecognitionRun: no ledger client configured")
		writeError(w, http.StatusServiceUnavailable, "ledger_client_not_configured", "")
		return
	}
	if run.JournalID != nil {
		if err := h.ledger.ReverseRecognitionJournal(r.Context(), tenantID, principalID, *run.JournalID, req.Reason); err != nil {
			h.log.Error("SupersedeRecognitionRun: journal reversal failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "journal_reversal_failed", err.Error())
			return
		}
	}
	now := time.Now().UTC()
	if err := h.store.SupersedeRecognitionRun(r.Context(), id, principalID, now); err != nil {
		h.writeRecognitionErr(w, err)
		return
	}
	run.Status, run.SupersededAt, run.SupersededByPrincipalID = domain.RecognitionRunStatusSuperseded, &now, &principalID
	h.publisher.PublishProjectRecognitionSuperseded(r.Context(), getCorrelationID(r), principalID, tenantID, *run)
	writeJSON(w, http.StatusOK, map[string]string{"run_id": id, "status": domain.RecognitionRunStatusSuperseded})
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

func (h *Handler) writeRecognitionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRecognitionRunNotFound):
		writeError(w, http.StatusNotFound, "recognition_run_not_found", "")
	case errors.Is(err, domain.ErrInvalidRecognitionRunTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrApprovedEstimateRequired):
		writeError(w, http.StatusUnprocessableEntity, "approved_estimate_required", err.Error())
	case errors.Is(err, domain.ErrContractValueRequired):
		writeError(w, http.StatusUnprocessableEntity, "contract_value_required", err.Error())
	default:
		h.log.Error("recognition store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
