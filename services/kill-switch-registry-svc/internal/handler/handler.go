package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/kill-switch-registry-svc/internal/authz"
	"zoiko.io/kill-switch-registry-svc/internal/domain"
	"zoiko.io/kill-switch-registry-svc/internal/events"
	svcmiddleware "zoiko.io/kill-switch-registry-svc/internal/middleware"
	"zoiko.io/kill-switch-registry-svc/internal/store"
)

// platformScopeID is the legal_entity_id passed to authorization-svc for a
// kill switch that is not tenant-scoped — same convention as
// commercial-account-svc/policy-svc/capability-registry-svc.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	KillSwitchEngage    = "KILL_SWITCH_ENGAGE"
	KillSwitchDisengage = "KILL_SWITCH_DISENGAGE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// resolveTenantScope returns the tenant dimension for a read, taken from
// the verified X-Tenant-Id rather than from the caller's query string.
//
// Both read routes used to read ?tenant_id= directly, so any caller could
// ask about any tenant: whether that tenant's commercial charging had been
// stopped, whether its automation was halted, and the reason text on the
// event. During an incident that is a live feed of another customer's
// operational trouble.
//
// A NIL return is legitimate here and is NOT the fail-closed case, which
// makes this different from every other service in this sweep. A nil tenant
// means "resolve the platform-level question", and under migration 000002's
// policy a caller with no verified tenant still sees every tenant_id IS
// NULL row — the platform-wide switches — and nobody's tenant-specific
// ones. That is the correct answer to the platform-level question, and it
// is why middleware.TenantContext here does not refuse tenant-less
// requests the way evidence-manifest-svc's does.
//
// A ?tenant_id= that disagrees with the verified header is refused rather
// than ignored, so a caller asking about someone else gets an error instead
// of a silently reinterpreted answer about itself.
func (h *Handler) resolveTenantScope(w http.ResponseWriter, r *http.Request, declared string) (*string, bool) {
	verified := svcmiddleware.TenantFromContext(r.Context())
	if declared != "" && declared != verified {
		writeError(w, http.StatusForbidden,
			"tenant_id does not match the verified X-Tenant-Id")
		return nil, false
	}
	return strPtrOrNil(verified), true
}

// authorize checks actionType against tenantID's scope if given, otherwise
// the platform scope — engaging/disengaging a tenant-scoped switch requires
// a role granted for that tenant specifically, same doctrine as every other
// service's per-scope authorize helper (e.g. commercial-account-svc).
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	scope := platformScopeID
	if tenantID != "" {
		scope = tenantID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/kill-switches", func(r chi.Router) {
		r.Post("/engage", h.EngageKillSwitch)
		r.Post("/disengage", h.DisengageKillSwitch)
		r.Get("/resolve", h.ResolveKillSwitch)
		r.Get("/", h.ListCurrentStates)
		r.Get("/history", h.ListHistoryForScope)
	})
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// EngageKillSwitch handles POST /v1/kill-switches/engage.
func (h *Handler) EngageKillSwitch(w http.ResponseWriter, r *http.Request) {
	var req domain.EngageKillSwitchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if req.ReconciliationProcedureRef == "" {
		writeError(w, http.StatusBadRequest, "reconciliation_procedure_ref is required to engage a kill switch")
		return
	}
	if req.ApprovedByPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "approved_by_principal_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.TenantID, KillSwitchEngage) {
		return
	}

	e := &domain.KillSwitchEvent{
		KillSwitchEventID:          uuid.NewString(),
		Plane:                      strPtrOrNil(req.Plane),
		Domain:                     strPtrOrNil(req.Domain),
		ProviderCode:               strPtrOrNil(req.ProviderCode),
		TenantID:                   strPtrOrNil(req.TenantID),
		Action:                     domain.KillSwitchActionEngage,
		Reason:                     req.Reason,
		ReconciliationProcedureRef: strPtrOrNil(req.ReconciliationProcedureRef),
		ApprovedByPrincipalID:      req.ApprovedByPrincipalID,
		CreatedAt:                  time.Now().UTC(),
		CreatedByPrincipalID:       principalID,
	}
	if err := h.store.AppendEvent(r.Context(), e); err != nil {
		h.logger.Error("engage kill switch failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to engage kill switch")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "kill_switch.engaged", EntityID: e.KillSwitchEventID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: req.CorrelationID, Payload: e,
	})
	writeJSON(w, http.StatusCreated, e)
}

// DisengageKillSwitch handles POST /v1/kill-switches/disengage. Requires the
// exact scope tuple currently be ENGAGED — disengaging an already-
// disengaged (or never-engaged) tuple is rejected, not silently accepted.
func (h *Handler) DisengageKillSwitch(w http.ResponseWriter, r *http.Request) {
	var req domain.DisengageKillSwitchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if req.ApprovedByPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "approved_by_principal_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.TenantID, KillSwitchDisengage) {
		return
	}

	plane, domainName, providerCode, tenantID := strPtrOrNil(req.Plane), strPtrOrNil(req.Domain), strPtrOrNil(req.ProviderCode), strPtrOrNil(req.TenantID)
	current, err := h.store.LatestEventForScope(r.Context(), plane, domainName, providerCode, tenantID)
	if err != nil {
		h.logger.Error("disengage kill switch: lookup failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to look up current kill switch state")
		return
	}
	if current == nil || current.Action != domain.KillSwitchActionEngage {
		writeError(w, http.StatusConflict, domain.ErrNotCurrentlyEngaged.Error())
		return
	}

	e := &domain.KillSwitchEvent{
		KillSwitchEventID:     uuid.NewString(),
		Plane:                 plane,
		Domain:                domainName,
		ProviderCode:          providerCode,
		TenantID:              tenantID,
		Action:                domain.KillSwitchActionDisengage,
		Reason:                req.Reason,
		ApprovedByPrincipalID: req.ApprovedByPrincipalID,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  principalID,
	}
	if err := h.store.AppendEvent(r.Context(), e); err != nil {
		h.logger.Error("disengage kill switch failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to disengage kill switch")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "kill_switch.disengaged", EntityID: e.KillSwitchEventID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: req.CorrelationID, Payload: e,
	})
	writeJSON(w, http.StatusOK, e)
}

// ResolveKillSwitch handles GET /v1/kill-switches/resolve?plane=&domain=&provider_code=&tenant_id=
// — the check every caller makes before a high-impact action. No authz
// gate: a read-only check every service needs to make cheaply and often,
// same posture as capability-registry-svc's resolution endpoint.
func (h *Handler) ResolveKillSwitch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	plane := strPtrOrNil(q.Get("plane"))
	domainName := strPtrOrNil(q.Get("domain"))
	providerCode := strPtrOrNil(q.Get("provider_code"))

	tenantID, ok := h.resolveTenantScope(w, r, q.Get("tenant_id"))
	if !ok {
		return
	}

	result, err := h.store.ResolveKillSwitch(r.Context(), plane, domainName, providerCode, tenantID)
	if err != nil {
		h.logger.Error("resolve kill switch failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to resolve kill switch")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ListCurrentStates handles GET /v1/kill-switches — every distinct scope
// tuple's current state, the "visible in operations" requirement.
func (h *Handler) ListCurrentStates(w http.ResponseWriter, r *http.Request) {
	states, err := h.store.ListCurrentStates(r.Context())
	if err != nil {
		h.logger.Error("list kill switch states failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list kill switch states")
		return
	}
	if states == nil {
		states = []domain.KillSwitchState{}
	}
	writeJSON(w, http.StatusOK, states)
}

// ListHistoryForScope handles GET /v1/kill-switches/history?plane=&domain=&provider_code=&tenant_id=
// — the full audit trail for one exact scope tuple.
func (h *Handler) ListHistoryForScope(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	plane := strPtrOrNil(q.Get("plane"))
	domainName := strPtrOrNil(q.Get("domain"))
	providerCode := strPtrOrNil(q.Get("provider_code"))

	tenantID, ok := h.resolveTenantScope(w, r, q.Get("tenant_id"))
	if !ok {
		return
	}

	history, err := h.store.ListHistoryForScope(r.Context(), plane, domainName, providerCode, tenantID)
	if err != nil {
		h.logger.Error("list kill switch history failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list kill switch history")
		return
	}
	if history == nil {
		history = []domain.KillSwitchEvent{}
	}
	writeJSON(w, http.StatusOK, history)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
