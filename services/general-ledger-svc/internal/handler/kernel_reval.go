package handler

import (
	"net/http"
	"time"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

func (h *KernelHandler) reval(w http.ResponseWriter, tenant string) (*kernel.RevalService, bool) {
	rr, ok := h.repo.(kernel.RevalRepo)
	src := h.fxSource(tenant)
	if !ok || src == nil {
		writeError(w, http.StatusNotImplemented, "revaluation_not_configured", "")
		return nil, false
	}
	return &kernel.RevalService{Repo: rr, Books: h.repo, Rates: src, Dir: kernel.TenantDirectory{Tenant: tenant, Ref: h.ref, Units: h.units, FX: src}, Gate: h.gate}, true
}

// RunRevaluation values the open foreign monetary items of a book at the closing rate and posts the unrealized difference, with its
// reversal on the first day of the next period. One run exists per entity, book and period: asking again returns it.
func (h *KernelHandler) RunRevaluation(w http.ResponseWriter, r *http.Request) {
	var req kernel.RevalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRevalRun)
	if !ok {
		return
	}
	svc, ok := h.reval(w, tenant)
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

// ListRevaluationRuns lists the revaluation runs of the tenant, optionally for one entity.
func (h *KernelHandler) ListRevaluationRuns(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionRevalRead)
	if !ok {
		return
	}
	svc, ok := h.reval(w, tenant)
	if !ok {
		return
	}
	list, err := svc.Repo.ListRevalRuns(r.Context(), tenant, r.URL.Query().Get("legal_entity_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "total": len(list)})
}

// GetForeignPositions lists the open foreign monetary positions of a book as of a date with their carrying amounts: what a
// subledger needs to state the carrying amount of an item it settles.
func (h *KernelHandler) GetForeignPositions(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionRevalRead)
	if !ok {
		return
	}
	svc, ok := h.reval(w, tenant)
	if !ok {
		return
	}
	q := r.URL.Query()
	asOf, err := time.Parse("2006-01-02", q.Get("as_of"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "as_of is a date, YYYY-MM-DD")
		return
	}
	bk, err := h.repo.GetBook(r.Context(), tenant, q.Get("book_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	pos, wm, err := svc.Repo.ForeignPositions(r.Context(), tenant, bk.LegalEntityID, bk.BookCode, bk.FunctionalCurrency, asOf)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of": q.Get("as_of"), "functional_currency": bk.FunctionalCurrency, "ledger_watermark": wm, "positions": pos})
}
