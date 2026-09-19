package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

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

	h.publisher.PublishReconciliationReperformed(r.Context(), *newRun, runID)
	writeJSON(w, http.StatusCreated, newRun)
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
