// COM-05 Platform Commercial Billing HTTP surface, part 5d (ZS-SVC-Q-001
// §4.5, §10).
package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Every command here is platform billing-operations authority — a dunning
// case tracks collections escalation, never a customer's own action, and a
// reconciliation run is an internal control, not a customer-facing report.
const (
	ActionDunningPolicyManage = "COMMERCIAL_DUNNING_POLICY_MANAGE"
	ActionDunningManage       = "COMMERCIAL_DUNNING_MANAGE"
	ActionDunningRead         = "COMMERCIAL_DUNNING_READ"
	ActionReconciliationRun   = "COMMERCIAL_RECONCILIATION_RUN"
	ActionReconciliationRead  = "COMMERCIAL_RECONCILIATION_READ"
)

const (
	CodeDunningPolicyNotFound     = "DUNNING_POLICY_NOT_FOUND"
	CodeDunningCaseNotFound       = "DUNNING_CASE_NOT_FOUND"
	CodeDunningCaseAlreadyOpen    = "DUNNING_CASE_ALREADY_OPEN_FOR_INVOICE"
	CodeDunningCaseInvalidState   = "DUNNING_CASE_INVALID_STATE"
	CodeDunningCaseFullyEscalated = "DUNNING_CASE_FULLY_ESCALATED"
	CodeReconciliationNotFound    = "RECONCILIATION_NOT_FOUND"
)

// dunningFailure maps COM-05 (part 5d) errors; writeFailure consults it last.
func dunningFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrDunningPolicyNotFound):
		return http.StatusUnprocessableEntity, CodeDunningPolicyNotFound, true
	case errors.Is(err, domain.ErrDunningCaseNotFound):
		return http.StatusNotFound, CodeDunningCaseNotFound, true
	case errors.Is(err, domain.ErrDunningCaseAlreadyOpenForInvoice):
		return http.StatusConflict, CodeDunningCaseAlreadyOpen, true
	case errors.Is(err, domain.ErrDunningCaseInvalidState):
		return http.StatusConflict, CodeDunningCaseInvalidState, true
	case errors.Is(err, domain.ErrDunningCaseFullyEscalated):
		return http.StatusConflict, CodeDunningCaseFullyEscalated, true
	case errors.Is(err, domain.ErrReconciliationNotFound):
		return http.StatusNotFound, CodeReconciliationNotFound, true
	}
	return 0, "", false
}

type DunningHandler struct {
	store  store.DunningStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewDunningHandler(st store.DunningStore, az AuthzChecker, logger *zap.Logger) *DunningHandler {
	return &DunningHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *DunningHandler) WithClock(now func() time.Time) *DunningHandler {
	h.now = now
	return h
}

func RegisterDunningRoutes(r chi.Router, h *DunningHandler) {
	r.Post("/v1/commercial/dunning-policies", h.PublishDunningPolicy)
	r.Post("/v1/commercial/invoices/{invoiceID}:start-dunning", h.StartDunning)
	r.Route("/v1/commercial/dunning-cases", func(r chi.Router) {
		r.Get("/{id}", h.GetDunningCase)
		r.Post("/{id}", h.DunningCaseAction)
	})
	r.Get("/v1/commercial/invoices/{invoiceID}/dunning-state", h.GetDunningState)
	r.Post("/v1/commercial/billing-accounts/{organizationID}:reconcile", h.ReconcileCommercialAccount)
	r.Get("/v1/commercial/billing-accounts/{organizationID}/reconciliation", h.GetCommercialReconciliation)
}

func (h *DunningHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *DunningHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

func (h *DunningHandler) authorizeAt(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

func (h *DunningHandler) sellerPrincipal(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	return principal, h.authorizeAt(w, r, principal, platformScopeID, action)
}

// readScope resolves who a read is about — see com05_credit_handler.go's
// identical comment: naming your own organization is still a self-read.
func (h *DunningHandler) readScope(w http.ResponseWriter, r *http.Request, orgFromPath, action string) (string, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return "", false
	}
	callerOrg := svcmiddleware.TenantFromContext(r.Context())
	if orgFromPath != "" && orgFromPath != callerOrg {
		if !h.authorizeAt(w, r, principal, platformScopeID, action) {
			return "", false
		}
		return orgFromPath, true
	}
	org := orgFromPath
	if org == "" {
		org = callerOrg
	}
	if org == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Tenant-Id is required"})
		return "", false
	}
	if !h.authorizeAt(w, r, principal, org, action) {
		return "", false
	}
	return org, true
}

// ── Dunning policy ───────────────────────────────────────────────────────────

type dunningPolicyRequest struct {
	PolicyVersion     int       `json:"policy_version"`
	Notice1AfterDays  int       `json:"notice1_after_days"`
	Notice2AfterDays  int       `json:"notice2_after_days"`
	RestrictAfterDays int       `json:"restrict_after_days"`
	SuspendAfterDays  int       `json:"suspend_after_days"`
	EffectiveFrom     time.Time `json:"effective_from"`
	Reason            string    `json:"reason"`
}

func (h *DunningHandler) PublishDunningPolicy(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionDunningPolicyManage)
	if !ok {
		return
	}
	var req dunningPolicyRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.PolicyVersion < 1 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "policy_version", Detail: "must be 1 or greater"})
		return
	}
	if req.EffectiveFrom.IsZero() {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "effective_from", Detail: "is required"})
		return
	}
	p := &domain.DunningPolicyVersion{PolicyVersion: req.PolicyVersion, Notice1AfterDays: req.Notice1AfterDays,
		Notice2AfterDays: req.Notice2AfterDays, RestrictAfterDays: req.RestrictAfterDays, SuspendAfterDays: req.SuspendAfterDays,
		EffectiveFrom: req.EffectiveFrom.UTC(), Reason: req.Reason, CreatedAt: h.now(), CreatedByPrincipalID: principal}
	if err := domain.ValidateDunningPolicyVersion(p); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "PublishDunningPolicy", "dunning-policy", raw, false)
	if !ok {
		return
	}
	got, err := h.store.PublishDunningPolicy(r.Context(), p, cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

// ── Dunning cases ────────────────────────────────────────────────────────────

func (h *DunningHandler) StartDunning(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionDunningManage)
	if !ok {
		return
	}
	if _, ok := readBody(w, r, &struct{}{}, true); !ok {
		return
	}
	id := domain.NewCommercialID(domain.PrefixDunningCase)
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "StartDunning", id, nil, false)
	if !ok {
		return
	}
	c, err := h.store.StartDunning(r.Context(), id, invoiceID, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

type stopDunningRequest struct {
	Reason string `json:"reason"`
}

// DunningCaseAction dispatches POST /dunning-cases/{id}:{advance|stop}.
func (h *DunningHandler) DunningCaseAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found || (action != "advance" && action != "stop") {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected :advance or :stop"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixDunningCase, rawID)
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionDunningManage)
	if !ok {
		return
	}
	var req stopDunningRequest
	raw, ok := readBody(w, r, &req, true)
	if !ok {
		return
	}
	if action == "stop" && strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "a reason is required to stop dunning"})
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "DunningCase:"+action, id, raw, false)
	if !ok {
		return
	}
	var c *domain.DunningCase
	var err error
	if action == "advance" {
		c, err = h.store.AdvanceDunning(r.Context(), id, principal, h.now(), cmd.claim)
	} else {
		c, err = h.store.StopDunning(r.Context(), id, principal, req.Reason, h.now(), cmd.claim)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *DunningHandler) GetDunningCase(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixDunningCase, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionDunningRead)
	if !ok {
		return
	}
	c, err := h.store.GetDunningCase(svcmiddleware.WithTenant(r.Context(), org), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *DunningHandler) GetDunningState(w http.ResponseWriter, r *http.Request) {
	invoiceID, ok := parseID(w, r, domain.PrefixInvoice, chi.URLParam(r, "invoiceID"))
	if !ok {
		return
	}
	org, ok := h.readScope(w, r, r.URL.Query().Get("organization_id"), ActionDunningRead)
	if !ok {
		return
	}
	c, err := h.store.GetDunningStateForInvoice(svcmiddleware.WithTenant(r.Context(), org), invoiceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoice_id": invoiceID, "open_case": c})
}

// ── Reconciliation ───────────────────────────────────────────────────────────

func (h *DunningHandler) ReconcileCommercialAccount(w http.ResponseWriter, r *http.Request) {
	org, err := parseUUID(chi.URLParam(r, "organizationID"))
	if err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "organizationID", Detail: "must be a UUID"})
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionReconciliationRun)
	if !ok {
		return
	}
	if _, ok := readBody(w, r, &struct{}{}, true); !ok {
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "ReconcileCommercialAccount", org, nil, false)
	if !ok {
		return
	}
	rec, err := h.store.ReconcileCommercialAccount(r.Context(), org, principal, h.now(), cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *DunningHandler) GetCommercialReconciliation(w http.ResponseWriter, r *http.Request) {
	orgFromPath := chi.URLParam(r, "organizationID")
	org, ok := h.readScope(w, r, orgFromPath, ActionReconciliationRead)
	if !ok {
		return
	}
	rec, err := h.store.GetCommercialReconciliation(svcmiddleware.WithTenant(r.Context(), org), org)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
