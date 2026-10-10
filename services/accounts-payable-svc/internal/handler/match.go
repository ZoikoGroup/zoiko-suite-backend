package handler

// AP-06 Invoice Matching HTTP surface (inside accounts-payable-svc; no new service).
//
//	POST /v1/invoices/{id}/match                      RunInvoiceMatch
//	POST /v1/invoices/{id}/match/reperform            ReperformInvoiceMatch
//	POST /v1/invoices/{id}/match/supersede            SupersedeMatchRun
//	GET  /v1/invoices/{id}/match                      GetMatchResult
//	GET  /v1/invoices/{id}/match/runs                 supersession chain
//	GET  /v1/invoices/{id}/match/lines/{line_id}      GetLineMatchDetail
//	GET  /v1/invoices/{id}/match/available-actions    GetAvailableActions
//	GET  /v1/match-exceptions?legal_entity_id=        ListMatchExceptions
//	POST /v1/match-exceptions/{id}/acknowledge        AcknowledgeMatchException
//	POST /v1/match-exceptions/{id}/route              RouteMatchException
//	POST /v1/match-exceptions/{id}/approve-variance   RecordApprovedVariance
//	GET  /v1/match-policies/{legal_entity_id}         GetMatchPolicyVersion (?version=)
//	POST /v1/match-policies                           new policy version (match.policy.manage)
//
// The handler gathers evidence and enforces authorization/SoD; domain.EvaluateMatch
// decides; the store persists the run, the exceptions and the invoice's match
// dimension in one transaction. The engine never approves, waives or mutates a
// source document.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/idem"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/purchaseorder"
	"zoiko.io/accounts-payable-svc/internal/receipts"
	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

const (
	actionMatchRun          = "INVOICE_MATCH_RUN"
	actionMatchRead         = "INVOICE_MATCH_READ"
	actionMatchResolve      = "INVOICE_MATCH_EXCEPTION_RESOLVE"
	actionMatchPolicyManage = "MATCH_POLICY_MANAGE"
)

// MatchStore is the AP-06 persistence contract.
type MatchStore interface {
	idem.Store
	GetMatchPolicy(ctx context.Context, tenantID, legalEntityID string, version int) (*domain.MatchPolicy, error)
	CreateMatchPolicy(ctx context.Context, tenantID string, p domain.MatchPolicy) (*domain.MatchPolicy, error)
	SaveMatchRun(ctx context.Context, in domain.SaveMatchRunInput) (*domain.MatchResultView, error)
	SupersedeMatchRun(ctx context.Context, in domain.SupersedeInput) (*domain.VendorInvoice, error)
	ResolveMatchException(ctx context.Context, a domain.ExceptionAction) (*domain.MatchExceptionRecord, *domain.VendorInvoice, error)
	GetMatchResult(ctx context.Context, tenantID, invoiceID string) (*domain.MatchResultView, error)
	ListMatchRuns(ctx context.Context, tenantID, invoiceID string) ([]domain.MatchRunRecord, error)
	ListMatchExceptions(ctx context.Context, f domain.ExceptionFilter) ([]domain.MatchExceptionRecord, error)
	GetMatchException(ctx context.Context, tenantID, exceptionID string) (*domain.MatchExceptionRecord, error)
}

// MatchDeps are the matching module's collaborators.
type MatchDeps struct {
	Store    MatchStore
	PO       purchaseorder.Reader
	Receipts receipts.Reader
}

// WithMatching enables the AP-06 endpoints. Without it they answer 503
// match_not_configured: matching fails closed rather than silently skipping.
func (h *Handler) WithMatching(d MatchDeps) *Handler {
	h.match = &d
	return h
}

func (h *Handler) registerInvoiceMatchRoutes(r chi.Router) {
	r.Route("/{invoice_id}/match", func(r chi.Router) {
		r.Post("/", h.idemCmd("RunInvoiceMatch", h.runMatch("RunInvoiceMatch")))
		r.Post("/reperform", h.idemCmd("ReperformInvoiceMatch", h.runMatch("ReperformInvoiceMatch")))
		r.Post("/supersede", h.idemCmd("SupersedeMatchRun", h.SupersedeMatchRun))
		r.Get("/", h.GetMatchResult)
		r.Get("/runs", h.ListMatchRuns)
		r.Get("/lines/{line_id}", h.GetLineMatchDetail)
		r.Get("/available-actions", h.GetMatchAvailableActions)
	})
}

func (h *Handler) registerMatchRoutes(r chi.Router) {
	r.Get("/v1/match-exceptions", h.ListMatchExceptions)
	r.Post("/v1/match-exceptions/{exception_id}/acknowledge", h.idemCmd("AcknowledgeMatchException", h.exceptionCmd(domain.ActionAcknowledge)))
	r.Post("/v1/match-exceptions/{exception_id}/route", h.idemCmd("RouteMatchException", h.exceptionCmd(domain.ActionRoute)))
	r.Post("/v1/match-exceptions/{exception_id}/approve-variance", h.idemCmd("RecordApprovedVariance", h.exceptionCmd(domain.ActionApproveVariance)))
	r.Get("/v1/match-policies/{legal_entity_id}", h.GetMatchPolicyVersion)
	r.Post("/v1/match-policies", h.CreateMatchPolicy)
}

// idemCmd wraps a command in the shared Idempotency-Key handling (a repeat returns
// the stored response with Idempotent-Replay: true; the same key on a different
// request is refused). The envelope middleware already demands the key on writes.
func (h *Handler) idemCmd(op string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.match == nil {
			writeError(w, http.StatusServiceUnavailable, "match_not_configured", "invoice matching is not configured on this instance")
			return
		}
		idem.Wrap(h.match.Store, svcmiddleware.TenantFromContext, op, false, fn)(w, r)
	}
}

// matchCtx resolves the pieces every match endpoint needs.
func (h *Handler) matchCtx(w http.ResponseWriter, r *http.Request) (tenant, principal string, ok bool) {
	if h.match == nil {
		writeError(w, http.StatusServiceUnavailable, "match_not_configured", "invoice matching is not configured on this instance")
		return "", "", false
	}
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "tenant_required", "X-Tenant-Id is required")
		return "", "", false
	}
	principal, ok = h.requirePrincipal(w, r)
	return tenant, principal, ok
}

// invoiceFor loads the invoice (tenant-scoped) and authorizes the action on its legal entity.
func (h *Handler) invoiceFor(w http.ResponseWriter, r *http.Request, principal, action string) (*domain.VendorInvoice, bool) {
	inv, err := h.store.GetInvoice(r.Context(), chi.URLParam(r, "invoice_id"))
	if err != nil {
		h.log.Error("match: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return nil, false
	}
	if inv == nil {
		writeError(w, http.StatusNotFound, "invoice_not_found", "")
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, inv.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return inv, true
}

func decodeOptional(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (h *Handler) matchErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMatchNotApplicable):
		writeAPIErr(w, newErr(http.StatusUnprocessableEntity, "match_not_applicable", domain.CodeMatchNotApplicable, err.Error()))
	case errors.Is(err, domain.ErrMatchInvoiceFinal):
		writeAPIErr(w, newErr(http.StatusConflict, "invoice_final", domain.CodeInvalidState, err.Error()))
	case errors.Is(err, domain.ErrMatchInvoiceState):
		writeAPIErr(w, newErr(http.StatusConflict, "invoice_not_matchable", domain.CodeInvalidState, err.Error()))
	case errors.Is(err, domain.ErrMatchRunNotFound):
		writeAPIErr(w, newErr(http.StatusNotFound, "match_run_not_found", domain.CodeNotFound, err.Error()))
	case errors.Is(err, domain.ErrMatchExceptionNotFound):
		writeAPIErr(w, newErr(http.StatusNotFound, "match_exception_not_found", domain.CodeNotFound, err.Error()))
	case errors.Is(err, domain.ErrMatchRunSuperseded):
		writeAPIErr(w, newErr(http.StatusConflict, "match_run_superseded", domain.CodeMatchRunSuperseded, err.Error()))
	case errors.Is(err, domain.ErrExceptionNotWaivable):
		writeAPIErr(w, newErr(http.StatusUnprocessableEntity, "exception_not_waivable", domain.CodeNotWaivable, err.Error()))
	case errors.Is(err, domain.ErrExceptionTransition):
		writeAPIErr(w, newErr(http.StatusConflict, "invalid_exception_state", domain.CodeInvalidState, err.Error()))
	case errors.Is(err, domain.ErrMatchSelfWaiver), errors.Is(err, domain.ErrMatchPolicySoD):
		writeAPIErr(w, newErr(http.StatusForbidden, "sod_conflict", domain.CodeSoDConflict, err.Error()))
	case errors.Is(err, domain.ErrMatchPolicyInvalid):
		writeAPIErr(w, newErr(http.StatusBadRequest, "invalid_policy", domain.CodeValidationFailed, err.Error()))
	default:
		h.mutationErr(w, err)
	}
}

// ── evidence ────────────────────────────────────────────────────────────────

// gatherEvidence reads the frozen inputs of one run. A dependency that cannot
// answer is recorded as MISSING evidence (the engine then yields INCOMPLETE): the
// outage is never read as "no difference".
func (h *Handler) gatherEvidence(ctx context.Context, caller svcclient.Caller, inv *domain.VendorInvoice, pol domain.MatchPolicy) (domain.MatchEvidence, *int) {
	m := h.match
	ev := domain.MatchEvidence{Invoice: *inv, Policy: pol, PriorInvoiced: map[string]float64{}}
	poID := ""
	if inv.PurchaseOrderID != nil {
		poID = *inv.PurchaseOrderID
	}
	ev.PO = domain.POEvidence{PurchaseOrderID: poID}
	var revision *int

	po, err := m.PO.GetOrder(ctx, caller, poID)
	if err != nil || (po.LegalEntityID != "" && po.LegalEntityID != inv.LegalEntityID) {
		ev.PO.Unavailable = true
	} else {
		ev.PO.Currency, ev.PO.Revision = po.CurrencyCode, po.Revision
		ev.PO.HasRevision, ev.PO.HasLines = po.HasRevision, po.HasLines
		for _, l := range po.Lines {
			ev.PO.Lines = append(ev.PO.Lines, domain.POLineEvidence{LineID: l.LineID, Quantity: l.Quantity, UnitPrice: l.UnitPrice})
		}
		if po.HasRevision {
			rev := po.Revision
			revision = &rev
		}
	}

	if prog, err := m.PO.OpenQuantity(ctx, caller, poID); err != nil {
		ev.PriorUnavail = true
	} else {
		for _, l := range prog {
			ev.PriorInvoiced[l.LineID] = l.InvoicedQuantity
		}
	}

	if pol.Mode == domain.MatchThreeWay {
		ev.Receipts = domain.ReceiptEvidence{Received: map[string]float64{}}
		if rtd, err := m.Receipts.ReceivedToDate(ctx, caller, poID); err != nil {
			ev.Receipts.Unavailable = true
		} else {
			ev.Receipts.HasLines = rtd.HasLines
			for _, l := range rtd.Lines {
				ev.Receipts.Received[l.POLineID] += l.ReceivedQuantity
			}
		}
	}
	return ev, revision
}

// ── commands ────────────────────────────────────────────────────────────────

type runRequest struct {
	ExpectedVersion *int `json:"expected_version,omitempty"`
}

func (h *Handler) runMatch(command string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, principal, ok := h.matchCtx(w, r)
		if !ok {
			return
		}
		inv, ok := h.invoiceFor(w, r, principal, actionMatchRun)
		if !ok {
			return
		}
		var req runRequest
		if err := decodeOptional(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if !inv.RequiresMatch() {
			h.matchErr(w, domain.ErrMatchNotApplicable)
			return
		}

		// The policy is resolved and FROZEN for this run. A legal entity that has
		// configured none gets the strict zero-tolerance default.
		pol := domain.DefaultMatchPolicy(inv.LegalEntityID)
		if p, err := h.match.Store.GetMatchPolicy(r.Context(), tenant, inv.LegalEntityID, 0); err == nil {
			pol = *p
		} else if !errors.Is(err, domain.ErrMatchPolicyNotFound) {
			h.matchErr(w, err)
			return
		}
		// A policy editor cannot certify results under their own policy version.
		if !pol.IsDefault && pol.CreatedBy == principal {
			h.matchErr(w, domain.ErrMatchPolicySoD)
			return
		}

		corr := r.Header.Get("X-Correlation-ID")
		caller := svcclient.Caller{TenantID: tenant, PrincipalID: principal, CorrelationID: corr, RequestID: r.Header.Get("X-Request-Id")}
		ev, rev := h.gatherEvidence(r.Context(), caller, inv, pol)
		outcome := domain.EvaluateMatch(ev)

		view, err := h.match.Store.SaveMatchRun(r.Context(), domain.SaveMatchRunInput{
			TenantID: tenant, InvoiceID: inv.InvoiceID, ExpectedVersion: req.ExpectedVersion, Command: command,
			Actor: principal, CorrelationID: corr, Policy: pol, PurchaseOrderID: ev.PO.PurchaseOrderID, PORevision: rev, Outcome: outcome,
		})
		if err != nil {
			h.matchErr(w, err)
			return
		}
		status := http.StatusOK
		if view.Created {
			status = http.StatusCreated
		}
		writeJSON(w, status, view)
	}
}

type supersedeRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Reason          string `json:"reason"`
}

func (h *Handler) SupersedeMatchRun(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	inv, ok := h.invoiceFor(w, r, principal, actionMatchResolve)
	if !ok {
		return
	}
	var req supersedeRequest
	if err := decodeOptional(r, &req); err != nil || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", "a reason is required to supersede a match run")
		return
	}
	out, err := h.match.Store.SupersedeMatchRun(r.Context(), domain.SupersedeInput{
		TenantID: tenant, InvoiceID: inv.InvoiceID, ExpectedVersion: req.ExpectedVersion, Actor: principal, Reason: req.Reason,
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		h.matchErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": out.InvoiceID, "match_state": out.MatchState, "cleared": out.MatchCleared, "version": out.Version})
}

type exceptionRequest struct {
	Reason    string `json:"reason"`
	RouteTo   string `json:"route_to,omitempty"`
	Reference string `json:"reference,omitempty"`
}

func (h *Handler) exceptionCmd(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, principal, ok := h.matchCtx(w, r)
		if !ok {
			return
		}
		id := chi.URLParam(r, "exception_id")
		exc, err := h.match.Store.GetMatchException(r.Context(), tenant, id)
		if err != nil {
			h.matchErr(w, err)
			return
		}
		if err := h.authz.CheckAllowed(r.Context(), principal, exc.LegalEntityID, actionMatchResolve); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
		var req exceptionRequest
		if err := decodeOptional(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		switch {
		case kind == domain.ActionApproveVariance && req.Reason == "":
			writeError(w, http.StatusBadRequest, "reason_required", "approving a variance requires a recorded reason")
			return
		case kind == domain.ActionRoute && (req.RouteTo == "" || req.Reason == ""):
			writeError(w, http.StatusBadRequest, "route_to_and_reason_required", "route_to and reason are required to route an exception")
			return
		}
		out, inv, err := h.match.Store.ResolveMatchException(r.Context(), domain.ExceptionAction{
			TenantID: tenant, ExceptionID: id, Kind: kind, Actor: principal, Reason: req.Reason, Ref: req.Reference, RouteTo: req.RouteTo,
			CorrelationID: r.Header.Get("X-Correlation-ID"),
		})
		if err != nil {
			h.matchErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"exception": out, "invoice_match_state": inv.MatchState, "cleared": inv.MatchCleared})
	}
}

type policyRequest struct {
	LegalEntityID      string           `json:"legal_entity_id"`
	Mode               domain.MatchMode `json:"mode"`
	QtyTolerancePct    float64          `json:"qty_tolerance_pct"`
	PriceTolerancePct  float64          `json:"price_tolerance_pct"`
	AmountToleranceAbs float64          `json:"amount_tolerance_abs"`
	Reason             string           `json:"reason"`
}

// CreateMatchPolicy appends a new immutable policy version (match.policy.manage).
// A policy takes effect for the NEXT run; it never re-reads a run already made.
func (h *Handler) CreateMatchPolicy(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	var req policyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "legal_entity_id and a valid body are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, req.LegalEntityID, actionMatchPolicyManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	p, err := h.match.Store.CreateMatchPolicy(r.Context(), tenant, domain.MatchPolicy{
		LegalEntityID: req.LegalEntityID, Mode: req.Mode, QtyTolerancePct: req.QtyTolerancePct, PriceTolerancePct: req.PriceTolerancePct,
		AmountToleranceAbs: req.AmountToleranceAbs, Reason: req.Reason, CreatedBy: principal,
	})
	if err != nil {
		h.matchErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ── queries ─────────────────────────────────────────────────────────────────

func (h *Handler) GetMatchResult(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	inv, ok := h.invoiceFor(w, r, principal, actionMatchRead)
	if !ok {
		return
	}
	view, err := h.match.Store.GetMatchResult(r.Context(), tenant, inv.InvoiceID)
	if err != nil {
		h.matchErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) ListMatchRuns(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	inv, ok := h.invoiceFor(w, r, principal, actionMatchRead)
	if !ok {
		return
	}
	runs, err := h.match.Store.ListMatchRuns(r.Context(), tenant, inv.InvoiceID)
	if err != nil {
		h.matchErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": inv.InvoiceID, "runs": runs, "count": len(runs)})
}

func (h *Handler) GetLineMatchDetail(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	inv, ok := h.invoiceFor(w, r, principal, actionMatchRead)
	if !ok {
		return
	}
	view, err := h.match.Store.GetMatchResult(r.Context(), tenant, inv.InvoiceID)
	if err != nil {
		h.matchErr(w, err)
		return
	}
	lineID := chi.URLParam(r, "line_id")
	for _, l := range view.Lines {
		if l.InvoiceLineID != lineID {
			continue
		}
		excs := []domain.MatchExceptionRecord{}
		for _, x := range view.Exceptions {
			if x.InvoiceLineID == lineID {
				excs = append(excs, x)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"run_id": view.Run.RunID, "line": l, "exceptions": excs})
		return
	}
	writeError(w, http.StatusNotFound, "match_line_not_found", "")
}

func (h *Handler) GetMatchAvailableActions(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	inv, ok := h.invoiceFor(w, r, principal, actionMatchRead)
	if !ok {
		return
	}
	actions := []string{}
	perException := map[string][]string{}
	if inv.RequiresMatch() && inv.ApprovalState != domain.ApprovalApproved &&
		inv.IntakeState != domain.IntakeQuarantined && inv.IntakeState != domain.IntakeRejected {
		view, err := h.match.Store.GetMatchResult(r.Context(), tenant, inv.InvoiceID)
		switch {
		case errors.Is(err, domain.ErrMatchRunNotFound):
			actions = append(actions, "RunInvoiceMatch")
		case err != nil:
			h.matchErr(w, err)
			return
		default:
			actions = append(actions, "ReperformInvoiceMatch")
			if view.Run.SupersededAt == nil {
				actions = append(actions, "SupersedeMatchRun")
				for _, x := range view.Exceptions {
					var a []string
					if x.Status == domain.ExceptionOpen || x.Status == domain.ExceptionRouted {
						a = append(a, "AcknowledgeMatchException")
					}
					if x.Status == domain.ExceptionOpen || x.Status == domain.ExceptionAcknowledged {
						a = append(a, "RouteMatchException")
					}
					if x.Waivable && x.Status != domain.ExceptionVarianceApproved {
						a = append(a, "RecordApprovedVariance")
					}
					if len(a) > 0 {
						perException[x.ExceptionID] = a
					}
				}
			} else {
				actions[0] = "RunInvoiceMatch"
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": inv.InvoiceID, "available_actions": actions, "exception_actions": perException})
}

func (h *Handler) ListMatchExceptions(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	le := r.URL.Query().Get("legal_entity_id")
	if le == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id_required", "legal_entity_id query parameter is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, le, actionMatchRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.match.Store.ListMatchExceptions(r.Context(), domain.ExceptionFilter{
		TenantID: tenant, LegalEntityID: le, Status: r.URL.Query().Get("status"), InvoiceID: r.URL.Query().Get("invoice_id"), Limit: limit,
	})
	if err != nil {
		h.matchErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list, "count": len(list)})
}

// GetMatchPolicyVersion returns a stored version (?version=N) or the latest; a legal
// entity with none configured sees the built-in strict default, marked is_default.
func (h *Handler) GetMatchPolicyVersion(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.matchCtx(w, r)
	if !ok {
		return
	}
	le := chi.URLParam(r, "legal_entity_id")
	if err := h.authz.CheckAllowed(r.Context(), principal, le, actionMatchRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	p, err := h.match.Store.GetMatchPolicy(r.Context(), tenant, le, version)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, p)
	case errors.Is(err, domain.ErrMatchPolicyNotFound) && version == 0:
		writeJSON(w, http.StatusOK, domain.DefaultMatchPolicy(le))
	case errors.Is(err, domain.ErrMatchPolicyNotFound):
		writeAPIErr(w, newErr(http.StatusNotFound, "match_policy_not_found", domain.CodeNotFound, err.Error()))
	default:
		h.matchErr(w, err)
	}
}
