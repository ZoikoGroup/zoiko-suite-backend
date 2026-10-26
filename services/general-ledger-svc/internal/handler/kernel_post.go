package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

// ClosePeriodGate is the kernel's period gate (standard §10) backed by financial-close-svc. It fails closed: when the close
// service cannot answer, nothing posts. A soft-closed period posts only with an explicit override that carries a reason and
// is separately authorized.
type ClosePeriodGate struct {
	Close close.Client
	Authz AuthZClient
}

// CheckPostable implements kernel.PeriodGate.
func (g ClosePeriodGate) CheckPostable(ctx context.Context, tenant, legalEntityID, fiscalPeriod string, postingDate time.Time, override bool, reason, principal string) error {
	err := g.Close.CheckPeriodOpenAt(ctx, tenant, close.PeriodRef{LegalEntityID: legalEntityID, PeriodName: fiscalPeriod, PostingDate: domain.Date{Time: postingDate}})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrSoftCloseOverrideRequired):
		if !override {
			return kernelExc(kernel.FamPeriod, "SOFT_CLOSE_OVERRIDE_REQUIRED", "period %s is in soft close; posting needs an explicit, authorized override with a reason", fiscalPeriod)
		}
		if aerr := g.Authz.CheckAllowed(ctx, principal, legalEntityID, actionOverrideSoftClose); aerr != nil {
			if errors.Is(aerr, domain.ErrAuthorizationDenied) {
				return kernelExc(kernel.FamAuth, "OVERRIDE_NOT_AUTHORIZED", "the soft-close override is not authorized for this principal")
			}
			return kernelExc(kernel.FamExt, "AUTHORIZATION_UNAVAILABLE", "authorization could not be checked")
		}
		return nil
	case errors.Is(err, domain.ErrPeriodHardClosed), errors.Is(err, domain.ErrPeriodLocked):
		return kernelExc(kernel.FamPeriod, "PERIOD_CLOSED", "period %s is closed for %s", fiscalPeriod, postingDate.Format("2006-01-02"))
	default:
		return kernelExc(kernel.FamExt, "PERIOD_SERVICE_UNAVAILABLE", "the period state could not be confirmed: %v", err)
	}
}

func kernelExc(family, code, format string, a ...any) error {
	return kernel.NewException(family, code, format, a...)
}

// ── POST /v1/accounting-postings ───────────────────────────────────────────────

// PostEvent is the kernel's posting command: it turns an accounting event into a posted journal in one book, atomically and
// exactly once per event, book and basis. A repeat answers 200 with the consequence that already exists.
func (h *KernelHandler) PostEvent(w http.ResponseWriter, r *http.Request) {
	var req kernel.PostRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionPostingExecute)
	if !ok {
		return
	}
	if h.gate == nil {
		writeError(w, http.StatusServiceUnavailable, "period_gate_not_configured", "")
		return
	}
	dir := kernel.TenantDirectory{Tenant: tenant, Ref: h.ref, Units: h.units, FX: h.fxSource(tenant)}
	res, err := h.svc.PostEvent(r.Context(), tenant, a, req, dir, h.gate)
	if err != nil {
		h.fail(w, err)
		return
	}
	if res.Existing {
		w.Header().Set("Idempotent-Replay", "true")
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// GetPostingTrace is the lineage of a posting (standard §32): by journal, accounting event or posting execution.
func (h *KernelHandler) GetPostingTrace(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionPostingView)
	if !ok {
		return
	}
	q := r.URL.Query()
	tq := kernel.TraceQuery{JournalID: q.Get("journal_id"), AccountingEventID: q.Get("accounting_event_id"), ExecutionID: q.Get("posting_execution_id")}
	n := 0
	for _, v := range []string{tq.JournalID, tq.AccountingEventID, tq.ExecutionID} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		writeError(w, http.StatusBadRequest, "one_identifier_required", "give exactly one of journal_id, accounting_event_id, posting_execution_id")
		return
	}
	tr, err := h.repo.PostingTrace(r.Context(), tenant, tq)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

// ── account scope: applicability and controls (standard §12) ───────────────────

type scopeStore interface {
	AccountApplicability(ctx context.Context, tenant, code string) (kernel.AccountScope, error)
	SetAccountScope(ctx context.Context, tenant, code string, sc kernel.AccountScope, actor kernel.Actor) error
}

type scopeBody struct {
	BookIDs                 []string `json:"book_ids"`
	CurrencyRestriction     string   `json:"currency_restriction"`
	ControlType             string   `json:"control_type"`
	CounterpartyRequired    bool     `json:"counterparty_required"`
	DirectPostingRestricted *bool    `json:"direct_posting_restricted,omitempty"`
}

// ControlTypes are the areas that can own a control account (§12).
var controlTypes = map[string]bool{"": true, "AR": true, "AP": true, "PAYROLL": true, "TAX": true, "INVENTORY": true, "ASSET": true, "BANK": true, "CLEARING": true, "INTERCOMPANY": true}

func (h *KernelHandler) scopes(w http.ResponseWriter) (scopeStore, bool) {
	s, ok := h.ref.(scopeStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "account_scope_not_configured", "")
	}
	return s, ok
}

// GetAccountScope reports where an account may be used and who owns it: the books it is valid in (none listed = every book),
// its currency restriction, its control type and whether its lines need a counterparty.
func (h *KernelHandler) GetAccountScope(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionViewAccount)
	if !ok {
		return
	}
	s, ok := h.scopes(w)
	if !ok {
		return
	}
	sc, err := s.AccountApplicability(r.Context(), tenant, chi.URLParam(r, "account_code"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if sc.BookIDs == nil {
		sc.BookIDs = []string{}
	}
	writeJSON(w, http.StatusOK, scopeBody{BookIDs: sc.BookIDs, CurrencyRestriction: sc.Currency, ControlType: sc.ControlType, CounterpartyRequired: sc.CounterpartyRequired})
}

// SetAccountScope sets an account's books, currency, control type and counterparty rule. An empty body lifts every restriction.
func (h *KernelHandler) SetAccountScope(w http.ResponseWriter, r *http.Request) {
	var req scopeBody
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionAmendAccount)
	if !ok {
		return
	}
	s, ok := h.scopes(w)
	if !ok {
		return
	}
	for _, b := range req.BookIDs {
		if _, err := uuid.Parse(b); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_book_id", "book_ids are accounting book ids")
			return
		}
	}
	if req.CurrencyRestriction != "" && !domain.ValidCurrencyCode(req.CurrencyRestriction) {
		writeError(w, http.StatusBadRequest, "invalid_currency_code", domain.ErrInvalidCurrency.Error())
		return
	}
	if !controlTypes[req.ControlType] {
		writeError(w, http.StatusBadRequest, "invalid_control_type", "control_type is AR, AP, PAYROLL, TAX, INVENTORY, ASSET, BANK, CLEARING or INTERCOMPANY")
		return
	}
	code := chi.URLParam(r, "account_code")
	sc := kernel.AccountScope{BookIDs: req.BookIDs, Currency: req.CurrencyRestriction, ControlType: req.ControlType, CounterpartyRequired: req.CounterpartyRequired, DirectPostingRestricted: req.DirectPostingRestricted}
	if err := s.SetAccountScope(r.Context(), tenant, code, sc, a); err != nil {
		h.fail(w, err)
		return
	}
	if req.BookIDs == nil {
		req.BookIDs = []string{}
	}
	writeJSON(w, http.StatusOK, req)
}
