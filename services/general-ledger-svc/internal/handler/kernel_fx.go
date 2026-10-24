package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// fxSource is where this tenant's exchange rates come from: a configured source, else the kernel's own rate store.
func (h *KernelHandler) fxSource(tenant string) kernel.FXSource {
	if h.fx != nil {
		return h.fx
	}
	if fr, ok := h.repo.(kernel.FXRepo); ok {
		return kernel.RateResolver{Repo: fr, Tenant: tenant}
	}
	return nil
}

func (h *KernelHandler) fxSvc(w http.ResponseWriter) (*kernel.FXService, bool) {
	fr, ok := h.repo.(kernel.FXRepo)
	if !ok {
		writeError(w, http.StatusNotImplemented, "fx_not_configured", "")
		return nil, false
	}
	return &kernel.FXService{Repo: fr}, true
}

type fxRateBody struct {
	Base         string `json:"base_currency"`
	Quote        string `json:"quote_currency"`
	Rate         string `json:"rate"`
	RateType     string `json:"rate_type"`
	EffectiveAt  string `json:"effective_at"`
	RateDate     string `json:"rate_date"`
	Source       string `json:"source"`
	SourceRateID string `json:"source_rate_id"`
	Reason       string `json:"reason"`
}

func parseWhen(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// IngestFXRate stores a sourced exchange rate. A rate is never edited: the same source reference again returns the stored rate,
// and a different rate under it is refused.
func (h *KernelHandler) IngestFXRate(w http.ResponseWriter, r *http.Request) {
	var b fxRateBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionFXRateManage)
	if !ok {
		return
	}
	svc, ok := h.fxSvc(w)
	if !ok {
		return
	}
	at, valid := parseWhen(b.EffectiveAt)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_field", "effective_at is an RFC 3339 time or a date")
		return
	}
	got, existing, err := svc.Ingest(r.Context(), tenant, a, kernel.FXRate{Base: strings.ToUpper(b.Base), Quote: strings.ToUpper(b.Quote), Rate: b.Rate, RateType: strings.ToUpper(b.RateType),
		EffectiveAt: at, RateDate: b.RateDate, Source: b.Source, SourceRateID: b.SourceRateID})
	if err != nil {
		h.fail(w, err)
		return
	}
	status := http.StatusCreated
	if existing {
		status = http.StatusOK
	}
	writeJSON(w, status, got)
}

func (h *KernelHandler) ListFXRates(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionFXRateRead)
	if !ok {
		return
	}
	svc, ok := h.fxSvc(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := kernel.FXFilter{Base: strings.ToUpper(q.Get("base_currency")), Quote: strings.ToUpper(q.Get("quote_currency")), RateType: strings.ToUpper(q.Get("rate_type")), Limit: queryLimit(r)}
	if v := q.Get("from"); v != "" {
		if t, ok := parseWhen(v); ok {
			f.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, ok := parseWhen(v); ok {
			f.To = &t
		}
	}
	list, err := svc.Repo.ListRates(r.Context(), tenant, f)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rates": list, "total": len(list)})
}

// ResolveFXRate answers what a posting would use: the rate the policy selects for a pair on a date, or the exception that says why none.
func (h *KernelHandler) ResolveFXRate(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionFXRateRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	d, valid := parseWhen(q.Get("date"))
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_field", "date is required")
		return
	}
	src := h.fxSource(tenant)
	if src == nil {
		writeError(w, http.StatusNotImplemented, "fx_not_configured", "")
		return
	}
	basis, err := src.Basis(r.Context(), q.Get("policy_id"), strings.ToUpper(q.Get("from")), strings.ToUpper(q.Get("to")), d)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, basis)
}

// RequestFXOverride asks for a hand-keyed rate; it is not a rate until a second person approves it.
func (h *KernelHandler) RequestFXOverride(w http.ResponseWriter, r *http.Request) {
	var b fxRateBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionFXRateManage)
	if !ok {
		return
	}
	svc, ok := h.fxSvc(w)
	if !ok {
		return
	}
	at, valid := parseWhen(b.EffectiveAt)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_field", "effective_at is an RFC 3339 time or a date")
		return
	}
	got, err := svc.RequestOverride(r.Context(), tenant, a, kernel.FXOverride{Base: strings.ToUpper(b.Base), Quote: strings.ToUpper(b.Quote), Rate: b.Rate, RateType: strings.ToUpper(b.RateType),
		EffectiveAt: at, RateDate: b.RateDate, Reason: b.Reason})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *KernelHandler) ListFXOverrides(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionFXRateRead)
	if !ok {
		return
	}
	svc, ok := h.fxSvc(w)
	if !ok {
		return
	}
	list, err := svc.Repo.ListOverrides(r.Context(), tenant, strings.ToUpper(r.URL.Query().Get("status")))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": list, "total": len(list)})
}

type decisionBody struct {
	Note string `json:"note"`
}

func (h *KernelHandler) decideFXOverride(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b decisionBody
		if !decodeJSON(w, r, &b) {
			return
		}
		tenant, a, ok := h.guard(w, r, actionFXRateApprove)
		if !ok {
			return
		}
		svc, ok := h.fxSvc(w)
		if !ok {
			return
		}
		o, rate, err := svc.Decide(r.Context(), tenant, a, chi.URLParam(r, "id"), approve, b.Note)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"override": o, "rate": rate})
	}
}
