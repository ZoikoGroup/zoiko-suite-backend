package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// Authorization actions of intercompany and consolidation (standard §25, §26). Preparing a consolidation and certifying it are
// different actions, held by different people.
const (
	actionConsolManage  = "GL_CONSOLIDATION_MANAGE"
	actionConsolRun     = "GL_CONSOLIDATION_RUN"
	actionConsolCertify = "GL_CONSOLIDATION_CERTIFY"
	actionConsolRead    = "GL_CONSOLIDATION_READ"
)

func (h *KernelHandler) consol(w http.ResponseWriter, tenant string) (*kernel.ConsolService, bool) {
	cr, ok1 := h.repo.(kernel.ConsolRepo)
	se, ok2 := h.repo.(kernel.SealRepo)
	snaps, ok3 := h.snapshots(w)
	src := h.fxSource(tenant)
	if !ok1 || !ok2 || src == nil {
		if ok3 {
			writeError(w, http.StatusNotImplemented, "consolidation_not_configured", "")
		}
		return nil, false
	}
	if !ok3 {
		return nil, false
	}
	return &kernel.ConsolService{Repo: cr, Books: h.repo, Snaps: snaps, Seals: se, Rates: src, Dir: kernel.TenantDirectory{Tenant: tenant, Ref: h.ref, Units: h.units, FX: src}, Gate: h.gate}, true
}

func (h *KernelHandler) CreateConsolidationGroup(w http.ResponseWriter, r *http.Request) {
	var req kernel.GroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolManage)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	g, err := svc.CreateGroup(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (h *KernelHandler) ListConsolidationGroups(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	list, err := svc.Repo.ListGroups(r.Context(), tenant)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": list, "total": len(list)})
}

func (h *KernelHandler) GetConsolidationGroup(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	g, err := svc.Repo.GetGroup(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (h *KernelHandler) AddConsolidationMember(w http.ResponseWriter, r *http.Request) {
	var req kernel.MemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolManage)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	m, err := svc.AddMember(r.Context(), tenant, a, chi.URLParam(r, "id"), req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *KernelHandler) EndConsolidationMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EffectiveTo string `json:"effective_to"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolManage)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	if err := svc.EndMember(r.Context(), tenant, a, chi.URLParam(r, "id"), chi.URLParam(r, "member"), req.EffectiveTo); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"member_id": chi.URLParam(r, "member"), "effective_to": req.EffectiveTo})
}

func (h *KernelHandler) PutConsolidationMapping(w http.ResponseWriter, r *http.Request) {
	var req kernel.MappingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolManage)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	m, err := svc.PutMapping(r.Context(), tenant, a, chi.URLParam(r, "id"), req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *KernelHandler) ListConsolidationMappings(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	list, err := svc.Repo.ListMappings(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": list, "total": len(list)})
}

// RunConsolidation freezes the members' populations, translates, eliminates and posts the group book. One live run exists per group
// and period: asking again returns it.
func (h *KernelHandler) RunConsolidation(w http.ResponseWriter, r *http.Request) {
	var req kernel.ConsolRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolRun)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
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

func (h *KernelHandler) ListConsolidationRuns(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	list, err := svc.Repo.ListConsolRuns(r.Context(), tenant, r.URL.Query().Get("group_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "total": len(list)})
}

func (h *KernelHandler) GetConsolidationRun(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	run, err := svc.Repo.GetConsolRun(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *KernelHandler) CertifyConsolidationRun(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionConsolCertify)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	run, err := svc.Certify(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *KernelHandler) SupersedeConsolidationRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolCertify)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	run, err := svc.Supersede(r.Context(), tenant, a, chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// GetIntercompanyPairing reports how the members' intercompany positions pair up (matched, amount difference, currency difference,
// unmatched) from the live ledgers, without freezing anything.
func (h *KernelHandler) GetIntercompanyPairing(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	q := r.URL.Query()
	res, err := svc.Pairing(r.Context(), tenant, q.Get("group_id"), q.Get("fiscal_period"), q.Get("as_of"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ProposeGroupAdjustment proposes a group policy adjustment: balanced, evidenced, inert until another person approves it.
func (h *KernelHandler) ProposeGroupAdjustment(w http.ResponseWriter, r *http.Request) {
	var req kernel.AdjustmentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolManage)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	req.GroupID = chi.URLParam(r, "id")
	adj, err := svc.CreateAdjustment(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, adj)
}

func (h *KernelHandler) ListGroupAdjustments(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionConsolRead)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	list, err := svc.Repo.ListAdjustments(r.Context(), tenant, chi.URLParam(r, "id"), r.URL.Query().Get("fiscal_period"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"adjustments": list, "total": len(list)})
}

func (h *KernelHandler) ApproveGroupAdjustment(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionConsolCertify)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	adj, err := svc.ApproveAdjustment(r.Context(), tenant, a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adj)
}

func (h *KernelHandler) WithdrawGroupAdjustment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionConsolManage)
	if !ok {
		return
	}
	svc, ok := h.consol(w, tenant)
	if !ok {
		return
	}
	adj, err := svc.WithdrawAdjustment(r.Context(), tenant, a, chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, adj)
}
