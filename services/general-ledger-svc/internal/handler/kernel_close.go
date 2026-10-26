package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// Authorization actions of period close and FX (standard §10, §23, §24). Sealing and reopening are separate actions, as are
// ingesting an exchange rate and approving an override of one.
const (
	actionPeriodSeal    = "GL_PERIOD_SEAL"
	actionPeriodReopen  = "GL_PERIOD_REOPEN"
	actionPeriodRead    = "GL_PERIOD_READ"
	actionFXRateManage  = "GL_FX_RATE_MANAGE"
	actionFXRateApprove = "GL_FX_RATE_APPROVE"
	actionFXRateRead    = "GL_FX_RATE_READ"
	actionRevalRun      = "GL_REVALUATION_RUN"
	actionRevalRead     = "GL_REVALUATION_READ"
	actionSnapshotSign  = "GL_SNAPSHOT_SIGN"
	actionSnapshotRead  = "GL_SNAPSHOT_READ"
	actionCloseEvidence = "GL_CLOSE_EVIDENCE_READ"
)

// guardFor is guard for an action on a named entity: the entity the caller acts on comes from the request, not from a header.
func (h *KernelHandler) guardFor(w http.ResponseWriter, r *http.Request, action, entity string) (tenant string, a kernel.Actor, ok bool) {
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return
	}
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if tenant == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entity, action); err != nil {
		if err == domain.ErrAuthorizationDenied || strings.Contains(err.Error(), domain.ErrAuthorizationDenied.Error()) {
			writeError(w, http.StatusForbidden, "authorization_denied", "")
		} else {
			h.log.Error("authorization check failed — failing closed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
		}
		return
	}
	return tenant, kernel.Actor{PrincipalID: principal, CorrelationID: r.Header.Get("X-Correlation-ID")}, true
}

func (h *KernelHandler) seals(w http.ResponseWriter) (kernel.SealRepo, bool) {
	s, ok := h.repo.(kernel.SealRepo)
	if !ok {
		writeError(w, http.StatusNotImplemented, "period_seals_not_configured", "")
	}
	return s, ok
}

type sealBody struct {
	LegalEntityID string `json:"legal_entity_id"`
	FiscalPeriod  string `json:"fiscal_period"`
	Action        string `json:"action"`
	AuthorityRef  string `json:"authority_ref"`
	Reason        string `json:"reason"`
}

// ApplyPeriodSeal seals a period (hard close), reopens it under named authority, or seals it again after the reopen. Once sealed,
// no posting transaction writes into the period; the period close service calls this as part of its own state change.
func (h *KernelHandler) ApplyPeriodSeal(w http.ResponseWriter, r *http.Request) {
	var b sealBody
	if !decodeJSON(w, r, &b) {
		return
	}
	b.Action = strings.ToUpper(strings.TrimSpace(b.Action))
	action := actionPeriodSeal
	switch b.Action {
	case kernel.SealActionSeal:
	case kernel.SealActionReopen, kernel.SealActionReseal:
		action = actionPeriodReopen
	default:
		writeError(w, http.StatusBadRequest, "invalid_action", "action is SEAL, REOPEN or RESEAL")
		return
	}
	if b.LegalEntityID == "" || strings.TrimSpace(b.FiscalPeriod) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id and fiscal_period are required")
		return
	}
	if b.Action == kernel.SealActionReopen && (strings.TrimSpace(b.AuthorityRef) == "" || strings.TrimSpace(b.Reason) == "") {
		writeError(w, http.StatusUnprocessableEntity, "authority_required", "reopening a sealed period needs the authority that permits it and a reason")
		return
	}
	tenant, a, ok := h.guardFor(w, r, action, b.LegalEntityID)
	if !ok {
		return
	}
	s, ok := h.seals(w)
	if !ok {
		return
	}
	seal, err := s.ApplySeal(r.Context(), tenant, b.LegalEntityID, strings.TrimSpace(b.FiscalPeriod), b.Action, a.PrincipalID, strings.TrimSpace(b.AuthorityRef), strings.TrimSpace(b.Reason), time.Now().UTC())
	if err != nil {
		h.fail(w, err)
		return
	}
	status := http.StatusOK
	if !seal.Existing && b.Action == kernel.SealActionSeal {
		status = http.StatusCreated
	}
	writeJSON(w, status, seal)
}

func (h *KernelHandler) GetPeriodSeal(w http.ResponseWriter, r *http.Request) {
	entity, period := chi.URLParam(r, "entity"), chi.URLParam(r, "period")
	tenant, _, ok := h.guardFor(w, r, actionPeriodRead, entity)
	if !ok {
		return
	}
	s, ok := h.seals(w)
	if !ok {
		return
	}
	seal, events, err := s.GetSeal(r.Context(), tenant, entity, period)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seal": seal, "history": events})
}

func (h *KernelHandler) ListPeriodSeals(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionPeriodRead)
	if !ok {
		return
	}
	s, ok := h.seals(w)
	if !ok {
		return
	}
	list, err := s.ListSeals(r.Context(), tenant, r.URL.Query().Get("legal_entity_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seals": list, "total": len(list)})
}
