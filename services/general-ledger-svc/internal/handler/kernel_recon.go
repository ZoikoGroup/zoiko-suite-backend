package handler

import (
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// Authorization actions of reconciliation (standard §27). Running, resolving and accepting an exception are separate actions: the
// person who accepts a break is never the one who raised it, and the database holds that rule too.
const (
	actionReconRun      = "GL_RECON_RUN"
	actionReconRead     = "GL_RECON_READ"
	actionReconResolve  = "GL_RECON_RESOLVE"
	actionReconApprove  = "GL_RECON_APPROVE"
	actionReconTolerate = "GL_RECON_TOLERANCE_MANAGE"
)

func (h *KernelHandler) recon(w http.ResponseWriter) (*kernel.ReconService, bool) {
	rr, ok := h.repo.(kernel.ReconRepo)
	if !ok {
		writeError(w, http.StatusNotImplemented, "reconciliation_not_configured", "")
		return nil, false
	}
	svc := &kernel.ReconService{Repo: rr, Books: h.repo, Signer: h.signer}
	if ss, ok := h.repo.(kernel.SnapshotRepo); ok {
		svc.Snapshots = ss
	}
	return svc, true
}

func (h *KernelHandler) RunReconciliation(w http.ResponseWriter, r *http.Request) {
	var req kernel.RunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionReconRun)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	run, err := svc.Run(r.Context(), tenant, a, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.observer().ReconciliationResult(run.Type, run.Status)
	writeJSON(w, http.StatusCreated, run)
}

func (h *KernelHandler) ListReconciliationRuns(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionReconRead)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := svc.Repo.ListRuns(r.Context(), tenant, kernel.ReconFilter{Type: strings.ToUpper(q.Get("reconciliation_type")), Status: strings.ToUpper(q.Get("status")),
		LegalEntityID: q.Get("legal_entity_id"), BookID: q.Get("book_id"), FiscalPeriod: q.Get("fiscal_period"), Limit: queryLimit(r)})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "total": len(list)})
}

func (h *KernelHandler) GetReconciliationRun(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionReconRead)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	run, err := svc.Repo.GetRun(r.Context(), tenant, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	events, err := svc.Repo.RunEvents(r.Context(), tenant, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "history": events})
}

func (h *KernelHandler) VerifyReconciliationRun(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionReconRead)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	res, err := svc.Verify(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type assignBody struct {
	Owner    string `json:"owner"`
	SLAHours int    `json:"sla_hours"`
}

func (h *KernelHandler) AssignReconciliationRun(w http.ResponseWriter, r *http.Request) {
	var b assignBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionReconResolve)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	run, err := svc.Assign(r.Context(), tenant, a, chi.URLParam(r, "id"), b.Owner, b.SLAHours)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

type acceptBody struct {
	Note     string   `json:"note"`
	Evidence []string `json:"evidence"`
}

func (h *KernelHandler) AcceptReconciliationRun(w http.ResponseWriter, r *http.Request) {
	var b acceptBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionReconApprove)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	run, err := svc.Accept(r.Context(), tenant, a, chi.URLParam(r, "id"), b.Note, b.Evidence)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

type resolveBody struct {
	ResolutionType string   `json:"resolution_type"`
	Note           string   `json:"note"`
	Evidence       []string `json:"evidence"`
	JournalID      string   `json:"journal_id"`
}

func (h *KernelHandler) ResolveReconciliationRun(w http.ResponseWriter, r *http.Request) {
	var b resolveBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionReconResolve)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	run, err := svc.Resolve(r.Context(), tenant, a, chi.URLParam(r, "id"), strings.ToUpper(b.ResolutionType), b.Note, b.Evidence, b.JournalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// GetReconciliationStatus is the reconciliation picture of one scope for the period-close gate: open cases, failed postings and
// which required reconciliations have not been run.
func (h *KernelHandler) GetReconciliationStatus(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionReconRead)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	bk, err := h.repo.GetBook(r.Context(), tenant, q.Get("book_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	period := q.Get("fiscal_period")
	if period == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "fiscal_period")
		return
	}
	cs, err := svc.Repo.CaseStatus(r.Context(), tenant, bk.LegalEntityID, bk.BookID, period)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

type toleranceBody struct {
	ReconciliationType string `json:"reconciliation_type"`
	LegalEntityID      string `json:"legal_entity_id"`
	MatchAmount        string `json:"match_amount"`
	WarningAmount      string `json:"warning_amount"`
	EffectiveFrom      string `json:"effective_from"`
}

func (h *KernelHandler) CreateReconciliationTolerance(w http.ResponseWriter, r *http.Request) {
	var b toleranceBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionReconTolerate)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	t := kernel.Tolerance{ReconciliationType: strings.ToUpper(b.ReconciliationType), LegalEntityID: b.LegalEntityID, MatchAmount: b.MatchAmount, WarningAmount: b.WarningAmount}
	if b.EffectiveFrom != "" {
		d, err := time.Parse("2006-01-02", b.EffectiveFrom)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "effective_from is a date, YYYY-MM-DD")
			return
		}
		t.EffectiveFrom = d
	}
	got, err := svc.CreateTolerance(r.Context(), tenant, a, t)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *KernelHandler) ListReconciliationTolerances(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionReconRead)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	list, err := svc.Repo.ListTolerances(r.Context(), tenant, strings.ToUpper(r.URL.Query().Get("reconciliation_type")))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tolerances": list, "total": len(list)})
}

// GetCounterpartyBalances lists the position of each customer or supplier on a control type as of a date, for comparing a
// subledger with the ledger one counterparty at a time. The balance is on the control type's normal side.
func (h *KernelHandler) GetCounterpartyBalances(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionLedgerQuery)
	if !ok {
		return
	}
	svc, ok := h.recon(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	control := strings.ToUpper(q.Get("control_type"))
	debitNormal := map[string]bool{"AR": true, "AP": false}
	if _, known := debitNormal[control]; !known {
		writeError(w, http.StatusBadRequest, "invalid_control_type", "control_type is AR or AP")
		return
	}
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
	list, err := svc.Repo.CounterpartyBalances(r.Context(), tenant, bk.LegalEntityID, bk.BookCode, control, asOf)
	if err != nil {
		h.fail(w, err)
		return
	}
	total := new(big.Rat)
	for i := range list {
		dr, _ := new(big.Rat).SetString(list[i].Debit)
		cr, _ := new(big.Rat).SetString(list[i].Credit)
		bal := new(big.Rat).Sub(dr, cr)
		if !debitNormal[control] {
			bal.Neg(bal)
		}
		list[i].Balance = bal.FloatString(2)
		total.Add(total, bal)
	}
	writeJSON(w, http.StatusOK, map[string]any{"control_type": control, "as_of": q.Get("as_of"), "currency": bk.FunctionalCurrency, "counterparties": list, "total_balance": total.FloatString(2)})
}

// ── standard rule packs ────────────────────────────────────────────────────────

func (h *KernelHandler) ListRulePacks(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.guard(w, r, actionRuleRead); !ok {
		return
	}
	type packView struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		EventTypes  []string `json:"event_types"`
		MappingKeys []string `json:"mapping_keys"`
	}
	out := []packView{}
	for _, p := range kernel.Packs() {
		keys := map[string]bool{}
		for _, ru := range p.Rules {
			for _, l := range ru.Content.LineTemplates {
				keys[l.Account.MappingKey] = true
			}
			if ru.Content.RealizedFX != nil {
				keys[ru.Content.RealizedFX.Gain.MappingKey] = true
				keys[ru.Content.RealizedFX.Loss.MappingKey] = true
			}
		}
		pv := packView{Name: p.Name, Description: p.Description, EventTypes: p.PackEventTypes(), MappingKeys: []string{}}
		for k := range keys {
			pv.MappingKeys = append(pv.MappingKeys, k)
		}
		out = append(out, pv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"packs": out})
}

type instantiateBody struct {
	Version       string `json:"version"`
	EffectiveFrom string `json:"effective_from"`
	BookType      string `json:"book_type"`
	FrameworkID   string `json:"accounting_framework_id"`
}

// InstantiateRulePack creates a pack's rules as DRAFT rule sets. They still need testing, approval by another person and
// activation by a third before anything posts: a pack is a starting point, not a shortcut around governance.
func (h *KernelHandler) InstantiateRulePack(w http.ResponseWriter, r *http.Request) {
	var b instantiateBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleManage)
	if !ok {
		return
	}
	pack, found := kernel.PackByName(chi.URLParam(r, "name"))
	if !found {
		writeError(w, http.StatusNotFound, "pack_not_found", "")
		return
	}
	from, err := time.Parse("2006-01-02", b.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "effective_from is a date, YYYY-MM-DD")
		return
	}
	drafts := pack.Drafts(b.Version, from, b.BookType, b.FrameworkID)
	created := []*kernel.RuleSet{}
	for _, d := range drafts {
		got, err := h.svc.CreateRuleSet(r.Context(), tenant, a, d)
		if err != nil {
			if errors.Is(err, kernel.ErrExists) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "already_exists", "detail": d.Name + " " + d.Version + " already exists", "created": created})
				return
			}
			h.fail(w, err)
			return
		}
		created = append(created, got)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pack": pack.Name, "rule_sets": created})
}
