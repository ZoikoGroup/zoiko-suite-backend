package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// Authorization actions of allocations (standard §21). Authoring a rule, approving it and running it are separate actions.
const (
	actionAllocManage  = "GL_ALLOCATION_MANAGE"
	actionAllocApprove = "GL_ALLOCATION_APPROVE"
	actionAllocRun     = "GL_ALLOCATION_RUN"
	actionAllocRead    = "GL_ALLOCATION_READ"
)

func (h *KernelHandler) alloc(w http.ResponseWriter, tenant string) (*kernel.AllocService, bool) {
	ar, ok := h.repo.(kernel.AllocRepo)
	if !ok {
		writeError(w, http.StatusNotImplemented, "allocations_not_configured", "")
		return nil, false
	}
	src := h.fxSource(tenant)
	return &kernel.AllocService{Repo: ar, Books: h.repo, Dir: kernel.TenantDirectory{Tenant: tenant, Ref: h.ref, Units: h.units, FX: src}, Gate: h.gate}, true
}

func (h *KernelHandler) CreateAllocationRule(w http.ResponseWriter, r *http.Request) {
	var req kernel.AllocRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionAllocManage)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	rule, err := svc.CreateRule(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (h *KernelHandler) ListAllocationRules(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionAllocRead)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	list, err := svc.Repo.ListAllocRules(r.Context(), tenant, r.URL.Query().Get("rule_code"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": list, "total": len(list)})
}

func (h *KernelHandler) GetAllocationRule(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionAllocRead)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	rule, err := svc.Repo.GetAllocRule(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *KernelHandler) ApproveAllocationRule(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionAllocApprove)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	rule, err := svc.ApproveRule(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *KernelHandler) RetireAllocationRule(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionAllocApprove)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	rule, err := svc.RetireRule(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// RunAllocation spreads a period pool over the recipients of a rule. One live run exists per rule, book and period.
func (h *KernelHandler) RunAllocation(w http.ResponseWriter, r *http.Request) {
	var req kernel.AllocRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionAllocRun)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	run, err := svc.Run(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	status := http.StatusCreated
	if run.Existing {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, status, run)
}

func (h *KernelHandler) ListAllocationRuns(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionAllocRead)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := svc.Repo.ListAllocRuns(r.Context(), tenant, q.Get("legal_entity_id"), q.Get("fiscal_period"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "total": len(list)})
}

func (h *KernelHandler) GetAllocationRun(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionAllocRead)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	run, err := svc.Repo.GetAllocRun(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *KernelHandler) ReverseAllocationRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionAllocRun)
	if !ok {
		return
	}
	svc, ok := h.alloc(w, tenant)
	if !ok {
		return
	}
	run, err := svc.Reverse(r.Context(), tenant, a, chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// GetBookComparison lays the consequences of one accounting event in each book side by side.
func (h *KernelHandler) GetBookComparison(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionPostingView)
	if !ok {
		return
	}
	id := r.URL.Query().Get("accounting_event_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "accounting_event_id_required", "")
		return
	}
	tr, err := h.repo.PostingTrace(r.Context(), tenant, kernel.TraceQuery{AccountingEventID: id})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, kernel.CompareBooks(tr))
}
