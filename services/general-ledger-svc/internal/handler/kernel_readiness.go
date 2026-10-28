package handler

import (
	"net/http"
	"time"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// GetRolloutReadiness reports whether the rules a tenant is about to run (a named standard pack, or every ACTIVE rule set)
// can resolve everything they name: account mapping keys, the accounts behind them and the dimensions they require. It is
// read-only. GET /v1/rollout-readiness?pack=<name>&on=YYYY-MM-DD
func (h *KernelHandler) GetRolloutReadiness(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionRuleRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	on := time.Now().UTC()
	if s := q.Get("on"); s != "" {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_date", "on must be YYYY-MM-DD")
			return
		}
		on = d
	}
	on = time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)

	var rules []kernel.RuleSet
	source := "active-rule-sets"
	if name := q.Get("pack"); name != "" {
		pack, found := kernel.PackByName(name)
		if !found {
			writeError(w, http.StatusNotFound, "pack_not_found", "")
			return
		}
		source = "pack:" + name
		rules = pack.Drafts("readiness", on, "", "")
	} else {
		list, err := h.repo.ListRuleSets(r.Context(), tenant, kernel.RuleFilter{Status: kernel.RuleActive, Limit: 500})
		if err != nil {
			h.fail(w, err)
			return
		}
		rules = list
	}
	rep, err := kernel.CheckReadiness(r.Context(), tenant, source, rules, h.ref, h.repo, on)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
