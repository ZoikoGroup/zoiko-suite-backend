package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

// ── POST /v1/reconciliation-runs ─────────────────────────────────────────────

// StartRun starts a new DRAFT reconciliation run for a bank account+date.
// Idempotent on (tenant, bank_account_id, statement_date, correlation_id).
func (h *Handler) StartRun(w http.ResponseWriter, r *http.Request) {
	var req domain.StartRunRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.BankAccountID == "" || req.StatementDate == "" || req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id, bank_account_id, statement_date are required")
		return
	}

	run, created, err := h.store.StartRun(r.Context(), tenantID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRunAlreadyExists) {
			writeError(w, http.StatusConflict, "run_already_exists", err.Error())
			return
		}
		h.writeStoreErr(w, "StartRun", err)
		return
	}

	h.publisher.PublishReconciliationStarted(r.Context(), *run)

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, run)
}

// ── GET /v1/reconciliation-runs/{run_id} ─────────────────────────────────────

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "run_not_found", "")
			return
		}
		h.writeStoreErr(w, "GetRun", err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ── POST /v1/reconciliation-runs/{run_id}/freeze ─────────────────────────────

// FreezePopulation snapshots the current statement lines for the run.
// Transitions run DRAFT→RUNNING. Idempotent: re-freeze returns the existing
// population unchanged.
func (h *Handler) FreezePopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	pop, created, err := h.store.FreezePopulation(r.Context(), tenantID, runID, principalID, correlationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRunNotFound):
			writeError(w, http.StatusNotFound, "run_not_found", "")
		case errors.Is(err, domain.ErrRunInvalidTransition):
			writeError(w, http.StatusConflict, "run_invalid_transition", err.Error())
		default:
			h.writeStoreErr(w, "FreezePopulation", err)
		}
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, pop)
}

// ── GET /v1/reconciliation-runs/{run_id}/population ──────────────────────────

func (h *Handler) GetRunPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "run_not_found", "")
			return
		}
		h.writeStoreErr(w, "GetRunPopulation", err)
		return
	}
	if run.PopulationID == nil {
		writeError(w, http.StatusNotFound, "population_not_frozen", "population has not been frozen for this run")
		return
	}
	pop, err := h.store.GetPopulation(r.Context(), tenantID, *run.PopulationID)
	if err != nil {
		if errors.Is(err, domain.ErrPopulationNotFound) {
			writeError(w, http.StatusNotFound, "population_not_found", "")
			return
		}
		h.writeStoreErr(w, "GetPopulation", err)
		return
	}
	writeJSON(w, http.StatusOK, pop)
}

// ── POST /v1/reconciliation-runs/{run_id}/certify ────────────────────────────

func (h *Handler) CertifyRun(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	// Load run for authz check.
	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "run_not_found", "")
			return
		}
		h.writeStoreErr(w, "CertifyRun/GetRun", err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionCertify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Wave 13: the doc's own SoD rule — "closed-period/control exceptions
	// need authorized remediation" — certification must fail closed on a
	// CLOSED/LOCKED period or an unreachable/ambiguous financial-close-svc
	// response, never default to "assume open."
	if err := h.checkPeriodOpenForCertification(r.Context(), tenantID, run); err != nil {
		switch {
		case errors.Is(err, domain.ErrPeriodLocked):
			writeError(w, http.StatusConflict, "period_locked", err.Error())
		case errors.Is(err, domain.ErrCloseServiceUnavailable):
			writeError(w, http.StatusServiceUnavailable, "close_service_unavailable", err.Error())
		default:
			h.writeStoreErr(w, "CertifyRun/CheckPeriodOpen", err)
		}
		return
	}

	cert, created, err := h.store.CertifyRun(r.Context(), tenantID, runID, principalID, correlationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRunNotFound):
			writeError(w, http.StatusNotFound, "run_not_found", "")
		case errors.Is(err, domain.ErrRunSuperseded):
			writeError(w, http.StatusConflict, "run_superseded", err.Error())
		case errors.Is(err, domain.ErrRunPopulationNotFrozen):
			writeError(w, http.StatusConflict, "population_not_frozen", err.Error())
		case errors.Is(err, domain.ErrRunInvalidTransition):
			writeError(w, http.StatusConflict, "run_invalid_transition", err.Error())
		case errors.Is(err, domain.ErrMaterialResidualBlocked):
			writeError(w, http.StatusUnprocessableEntity, "material_residual_blocked", err.Error())
		case errors.Is(err, domain.ErrRunSelfCertificationForbidden):
			writeError(w, http.StatusConflict, "self_certification_forbidden", err.Error())
		default:
			h.writeStoreErr(w, "CertifyRun", err)
		}
		return
	}

	h.publisher.PublishReconciliationCertified(r.Context(), *run, *cert)

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, cert)
}

// checkPeriodOpenForCertification derives the doc-standard "YYYY-MM"
// period_name from the run's own statement_date (not "now" — the period
// being certified is the one the statement belongs to) and asks
// financial-close-svc whether it's open. A closeClient that isn't
// configured (nil) fails closed, same as an unreachable one — there is
// no honest "skip the check" default for a financial control gate.
func (h *Handler) checkPeriodOpenForCertification(ctx context.Context, tenantID string, run *domain.ReconciliationRun) error {
	if h.closeClient == nil {
		h.log.Error("CertifyRun: financial-close-svc client not configured — failing closed")
		return domain.ErrCloseServiceUnavailable
	}
	statementDate, err := time.Parse("2006-01-02", run.StatementDate)
	if err != nil {
		h.log.Error("CertifyRun: run's statement_date did not parse — failing closed", zap.String("statement_date", run.StatementDate), zap.Error(err))
		return domain.ErrCloseServiceUnavailable
	}
	periodName := statementDate.Format("2006-01")
	return h.closeClient.CheckPeriodOpen(ctx, tenantID, run.LegalEntityID, periodName)
}

// ── POST /v1/reconciliation-runs/{run_id}/reperform ──────────────────────────

// ReperformRun supersedes the existing run (preserving it and its certificate)
// and creates a new DRAFT run for the same account+date. The caller is
// expected to freeze the new run's population and re-run matching.
func (h *Handler) ReperformRun(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "run_not_found", "")
			return
		}
		h.writeStoreErr(w, "ReperformRun/GetRun", err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	newRun, err := h.store.SupersedeRun(r.Context(), tenantID, runID, principalID, correlationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRunNotFound):
			writeError(w, http.StatusNotFound, "run_not_found", "")
		case errors.Is(err, domain.ErrRunSuperseded):
			writeError(w, http.StatusConflict, "run_superseded", "run is already superseded; use the current run")
		default:
			h.writeStoreErr(w, "ReperformRun", err)
		}
		return
	}

	h.publisher.PublishReconciliationSuperseded(r.Context(), *run, newRun.RunID)
	h.publisher.PublishReconciliationReperformed(r.Context(), *newRun, runID)
	writeJSON(w, http.StatusCreated, newRun)
}

// ── POST /v1/reconciliation-runs/{run_id}/auto-match ─────────────────────────
//
// RunAutomaticMatching is BNK-05's batch matcher: for each caller-supplied
// candidate it runs the exact same deterministic verification as the
// manual single-actor match path (verifyJournalMatches /
// verifyCanonicalMatch) and, only on a pass, applies the match directly —
// no separate propose/confirm step, because the "checker" here is the
// independent cross-service verification against general-ledger-svc or
// banking-connector-svc, not a second human's judgment call.
//
// Every candidate is checked against ListUnmatchedLinesInPopulation
// first — a statement_line_id that isn't a genuinely UNMATCHED line in
// THIS run's frozen population is skipped with a reason, never matched,
// even if it exists and is UNMATCHED elsewhere. One candidate failing
// verification or already being resolved never aborts the batch; every
// candidate gets its own result.
func (h *Handler) RunAutomaticMatching(w http.ResponseWriter, r *http.Request) {
	var req domain.RunAutomaticMatchingRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")

	run, err := h.store.GetRun(r.Context(), tenantID, runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "run_not_found", "")
			return
		}
		h.writeStoreErr(w, "RunAutomaticMatching/GetRun", err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionMatch); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if run.PopulationID == nil {
		writeError(w, http.StatusConflict, "population_not_frozen", domain.ErrRunPopulationNotFrozen.Error())
		return
	}

	unmatched, err := h.store.ListUnmatchedLinesInPopulation(r.Context(), tenantID, *run.PopulationID)
	if err != nil {
		h.writeStoreErr(w, "RunAutomaticMatching/ListUnmatchedLinesInPopulation", err)
		return
	}
	byLineID := make(map[string]domain.StatementLine, len(unmatched))
	for _, l := range unmatched {
		byLineID[l.StatementLineID] = l
	}

	resp := domain.RunAutomaticMatchingResponse{RunID: runID}
	for _, c := range req.Candidates {
		result := domain.AutoMatchResult{StatementLineID: c.StatementLineID}

		l, inPopulation := byLineID[c.StatementLineID]
		switch {
		case !inPopulation:
			result.Reason = "not an UNMATCHED line in this run's frozen population"
		case c.JournalID == "" && c.TransactionID == "":
			result.Reason = "missing journal_id or transaction_id"
		case c.TransactionID != "":
			if h.banking == nil {
				result.Reason = "banking connector integration not configured"
				break
			}
			if err := h.verifyCanonicalMatch(r.Context(), l, c.TransactionID); err != nil {
				result.Reason = err.Error()
				break
			}
			if err := h.store.MatchStatementLineWithCanonical(r.Context(), tenantID, c.StatementLineID, c.TransactionID, principalID); err != nil {
				result.Reason = err.Error()
				break
			}
			l.Status = domain.StatementLineStatusMatched
			l.MatchedTransactionID = &c.TransactionID
			l.MatchedByPrincipalID = &principalID
			h.publisher.PublishReconciliationMatched(r.Context(), l)
			result.Matched = true
		default:
			if err := h.verifyJournalMatches(r.Context(), l, c.JournalID); err != nil {
				result.Reason = err.Error()
				break
			}
			if err := h.store.MatchStatementLine(r.Context(), tenantID, c.StatementLineID, c.JournalID, principalID); err != nil {
				result.Reason = err.Error()
				break
			}
			l.Status = domain.StatementLineStatusMatched
			l.MatchedJournalID = &c.JournalID
			l.MatchedByPrincipalID = &principalID
			h.publisher.PublishReconciliationMatched(r.Context(), l)
			result.Matched = true
		}

		if result.Matched {
			resp.MatchedCount++
		} else {
			resp.SkippedCount++
			h.log.Info("RunAutomaticMatching: candidate skipped",
				zap.String("run_id", runID), zap.String("statement_line_id", c.StatementLineID), zap.String("reason", result.Reason))
		}
		resp.Results = append(resp.Results, result)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── POST /v1/reconciliation-runs/{run_id}/bind-policy ────────────────────────

func (h *Handler) BindPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PolicyID string `json:"policy_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	runID := chi.URLParam(r, "run_id")
	if req.PolicyID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "policy_id")
		return
	}
	run, err := h.store.BindPolicy(r.Context(), tenantID, runID, req.PolicyID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRunNotFound):
			writeError(w, http.StatusNotFound, "run_not_found", "")
		case errors.Is(err, domain.ErrPolicyNotFound):
			writeError(w, http.StatusNotFound, "policy_not_found", "")
		case errors.Is(err, domain.ErrRunInvalidTransition):
			writeError(w, http.StatusConflict, "run_invalid_transition", "run must be in DRAFT status to bind a policy")
		default:
			h.writeStoreErr(w, "BindPolicy", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ── POST /v1/reconciliation-policies ─────────────────────────────────────────

func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePolicyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.LegalEntityID == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id, effective_from are required")
		return
	}
	pol, err := h.store.CreatePolicy(r.Context(), tenantID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyWouldWidenActiveRun) {
			writeError(w, http.StatusConflict, "policy_would_widen_active_run", err.Error())
			return
		}
		h.writeStoreErr(w, "CreatePolicy", err)
		return
	}
	writeJSON(w, http.StatusCreated, pol)
}

// ── GET /v1/reconciliation-policies/current ───────────────────────────────────

func (h *Handler) GetCurrentPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id query param required")
		return
	}
	pol, err := h.store.GetCurrentPolicy(r.Context(), tenantID, legalEntityID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			writeError(w, http.StatusNotFound, "policy_not_found", "")
			return
		}
		h.writeStoreErr(w, "GetCurrentPolicy", err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}
