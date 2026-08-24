package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/jurisdiction"
	"zoiko.io/authorization-svc/internal/siem"
)

// AuthorizationStore is the narrow interface the handler depends on.
type AuthorizationStore interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	SetRoleActive(ctx context.Context, roleID, tenantID string, active bool) (*domain.Role, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error)
	CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)
	CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error)
	FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID string) (*domain.DelegatedAuthority, error)
	RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID string) (*domain.DelegatedAuthority, error)
	CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error)
	FindGrantedActions(ctx context.Context, principalID, legalEntityID string) ([]string, string, error)
	FindDelegatedActions(ctx context.Context, principalID, legalEntityID string) ([]string, string, error)
	CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction, tenantID string) (string, bool, error)
	RecordAccessDecision(ctx context.Context, params domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error)
	FindAccessDecisionByID(ctx context.Context, accessDecisionID, tenantID string) (*domain.AccessDecisionLog, error)
}

// EventPublisher is the narrow interface the handler depends on.
type EventPublisher interface {
	PublishAuthorizationGranted(ctx context.Context, d domain.AccessDecisionLog) error
	PublishAuthorizationDenied(ctx context.Context, d domain.AccessDecisionLog) error
	PublishSoDViolationDetected(ctx context.Context, d domain.AccessDecisionLog, conflictingAction string) error
}

type Handler struct {
	store                 AuthorizationStore
	publisher             EventPublisher
	jurisdictionValidator jurisdiction.Validator
	siem                  *siem.Client
	log                   *zap.Logger

	// platformScopeEntityID is the synthetic legal entity a platform-wide act
	// is authorized against — see requirePlatformAction. Empty refuses every
	// platform-wide act, which is the correct default for a control that has
	// not been provisioned yet.
	platformScopeEntityID string
}

func New(store AuthorizationStore, publisher EventPublisher, jurisdictionValidator jurisdiction.Validator, siemClient *siem.Client, platformScopeEntityID string, log *zap.Logger) *Handler {
	return &Handler{
		store:                 store,
		publisher:             publisher,
		jurisdictionValidator: jurisdictionValidator,
		siem:                  siemClient,
		platformScopeEntityID: platformScopeEntityID,
		log:                   log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/admin/roles", h.CreateRole)
	r.Post("/v1/admin/roles/{role_id}/retire", h.RetireRole)
	r.Post("/v1/admin/roles/{role_id}/reactivate", h.ReactivateRole)
	r.Post("/v1/admin/roles/{role_id}/permission-bundles", h.CreatePermissionBundle)
	r.Post("/v1/admin/role-assignments", h.CreateRoleAssignment)
	r.Post("/v1/admin/role-assignments/{assignment_id}/revoke", h.RevokeRoleAssignment)
	r.Post("/v1/admin/delegated-authorities", h.CreateDelegatedAuthority)
	r.Post("/v1/admin/delegated-authorities/{delegation_id}/revoke", h.RevokeDelegatedAuthority)
	r.Post("/v1/admin/sod-rules", h.CreateSoDRule)

	r.Post("/v1/authorize", h.Authorize)
	r.Get("/v1/access-decisions/{access_decision_id}", h.GetAccessDecision)
}

// requirePrincipal reads the caller's verified principal from the
// X-Principal-Id header the gateway sets from a verified identity
// envelope, rejecting the request if absent.
//
// Before this fix, none of the /v1/admin/* routes checked this at all:
// created_by_principal_id / assigned_by / delegator_principal_id all came
// straight from the request body, on the platform's own authorization
// engine — the same "attribution taken from the request body defeats
// segregation of duties" shape already found and fixed in
// board-resolutions-svc.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_principal",
			"message": "X-Principal-Id is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return principalID, true
}

// requireTenant reads the caller's verified tenant scope from the
// X-Tenant-Id header, rejecting the request if absent.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return tenantID, true
}

// refuseForeignTenant reports whether claimed names a tenant other than
// verifiedTenant, writing a 403 if so. Before this fix, every /v1/admin/*
// route trusted whatever tenant_id the request body supplied outright —
// any caller could create a role, grant a permission bundle, or write a
// SoD rule into any tenant, purely by naming it in the JSON body.
func (h *Handler) refuseForeignTenant(w http.ResponseWriter, claimed, verifiedTenant string) bool {
	if claimed != "" && claimed != verifiedTenant {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "request tenant_id does not match the caller's verified tenant scope",
		})
		return true
	}
	return false
}

// requirePlatformAction confirms the caller holds actionType at platform
// scope, and records the decision like any other.
//
// This service is the authorization engine, so it does not call itself over
// HTTP — it asks its own store the same question /v1/authorize asks. What it
// asks about is the synthetic platform-scope legal entity (config
// AUTHZ_PLATFORM_SCOPE_ENTITY_ID, the same pattern policy-svc uses for a
// policy with no owning entity), because a platform-wide act has no legal
// entity of its own.
//
// Fails closed in both directions: an unset platform-scope entity id refuses
// every platform-wide act rather than waving them through, and a store error
// is a refusal, not an allow. A grant here is a distinct grant — it is
// deliberately NOT satisfied by any tenant-level role.
func (h *Handler) requirePlatformAction(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	correlationID := r.Header.Get("X-Correlation-ID")

	if h.platformScopeEntityID == "" {
		h.log.Error("platform-scope action refused: AUTHZ_PLATFORM_SCOPE_ENTITY_ID is not configured",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType))
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "platform_scope_not_configured",
			"message": "platform-wide actions require AUTHZ_PLATFORM_SCOPE_ENTITY_ID to be configured",
		})
		return false
	}

	actions, basis, err := h.store.FindGrantedActions(r.Context(), principalID, h.platformScopeEntityID)
	if err != nil {
		h.log.Error("platform-scope action check failed — refusing",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}

	outcome, decisionBasis := "DENIED", "no_grant"
	if contains(actions, actionType) {
		outcome, decisionBasis = "GRANTED", basis
	}

	// A platform-wide governance act is a material action, so it gets a
	// decision artifact whichever way it goes — the same constraint
	// /v1/authorize is held to. A failure to record is not a reason to
	// proceed: it is logged and the act is refused below if denied, and
	// allowed only when the decision was recorded.
	if _, recErr := h.store.RecordAccessDecision(r.Context(), domain.RecordAccessDecisionParams{
		PrincipalID:   principalID,
		LegalEntityID: h.platformScopeEntityID,
		ActionType:    actionType,
		Outcome:       outcome,
		Basis:         decisionBasis,
		CorrelationID: correlationID,
	}); recErr != nil {
		h.log.Error("platform-scope action: failed to record decision — refusing",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType),
			zap.Error(recErr))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}

	if outcome != "GRANTED" {
		h.siem.Stream(r.Context(), "", "authorization.denied", siem.SeverityHigh,
			"Platform-scope action "+actionType+" denied for principal "+principalID)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "authorization_denied",
			"message": actionType + " is required to act at platform scope",
		})
		return false
	}
	return true
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/admin/roles ─────────────────────────────────────────────────────

type createRoleRequest struct {
	RoleID        string `json:"role_id,omitempty"`
	TenantID      string `json:"tenant_id"`
	RoleCode      string `json:"role_code"`
	RoleName      string `json:"role_name"`
	RoleScopeType string `json:"role_scope_type"`
}

func (req createRoleRequest) missingField() string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.RoleCode == "":
		return "role_code"
	case req.RoleName == "":
		return "role_name"
	case req.RoleScopeType == "":
		return "role_scope_type"
	default:
		return ""
	}
}

// CreateRole handles POST /v1/admin/roles. Idempotent on (tenant_id, role_code).
//
// Response: 201 created / 200 idempotent replay / 400 missing field / 409 conflict / 503 unavailable.
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	role, created, err := h.store.CreateRole(r.Context(), domain.CreateRoleParams{
		// created_by_principal_id is always the verified caller, never the
		// request body — see requirePrincipal's doc comment.
		RoleID: req.RoleID, TenantID: req.TenantID, RoleCode: req.RoleCode,
		RoleName: req.RoleName, RoleScopeType: req.RoleScopeType, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "role_conflict", "role_code": req.RoleCode})
			return
		}
		h.log.Error("CreateRole: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, role)
}

// ── POST /v1/admin/roles/{role_id}/retire | /reactivate ──────────────────────

// RetireRole handles POST /v1/admin/roles/{role_id}/retire.
//
// Sets active_flag false, which is what actually stops the role granting
// anything: FindGrantedActions joins through `roles.active_flag`, so every
// action this role conferred disappears from the next /v1/authorize decision
// for every principal holding it. Assignments are left intact so the effect is
// reversible — see SetRoleActive for why cascading revocation is a separate
// decision.
//
// Idempotent: retiring an already-retired role is 200, not 409. The caller
// asked for a state, and that state holds.
//
// Response: 200 retired / 401 missing principal or tenant scope /
// 404 role not found or owned by another tenant / 503 unavailable.
func (h *Handler) RetireRole(w http.ResponseWriter, r *http.Request) {
	h.setRoleActive(w, r, false)
}

// ReactivateRole handles POST /v1/admin/roles/{role_id}/reactivate.
//
// Restores exactly the access the retirement suspended, because the
// assignments were never removed. Response shape matches RetireRole.
func (h *Handler) ReactivateRole(w http.ResponseWriter, r *http.Request) {
	h.setRoleActive(w, r, true)
}

func (h *Handler) setRoleActive(w http.ResponseWriter, r *http.Request, active bool) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	// This pair of routes checked NOTHING before: not the principal, not the
	// tenant, not who owns the role. An unauthenticated POST could retire any
	// tenant's role by id — stripping every principal holding it of every
	// action it grants, since FindGrantedActions joins through
	// roles.active_flag — or reactivate one that had been deliberately
	// retired. Seven sibling admin routes already required both headers.
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// Same doctrine as CreatePermissionBundle and CreateRoleAssignment: the
	// role's OWN tenant decides scope, and it must be the caller's. 404 rather
	// than 403 so a probe against another tenant's role_id cannot confirm it
	// exists.
	role, err := h.store.FindRoleByID(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
			return
		}
		h.log.Error("setRoleActive: role lookup failed",
			zap.String("correlation_id", correlationID),
			zap.String("role_id", roleID),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if role.TenantID != tenantScope {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
		return
	}

	role, err = h.store.SetRoleActive(r.Context(), roleID, tenantScope, active)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found"})
			return
		}
		h.log.Error("setRoleActive: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.String("role_id", roleID),
			zap.Bool("active", active),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Logged at info because this is a change to what the platform will
	// enforce, not a read. A retirement that nobody can account for later is
	// the same problem as one that never happened.
	h.log.Info("role active_flag changed",
		zap.String("correlation_id", correlationID),
		zap.String("role_id", role.RoleID),
		zap.String("role_code", role.RoleCode),
		zap.String("tenant_id", role.TenantID),
		zap.Bool("active_flag", role.ActiveFlag))

	writeJSON(w, http.StatusOK, role)
}

// ── POST /v1/admin/roles/{role_id}/permission-bundles ───────────────────────

type createBundleRequest struct {
	BundleCode       string   `json:"bundle_code"`
	PermittedActions []string `json:"permitted_actions"`
}

// CreatePermissionBundle handles POST /v1/admin/roles/{role_id}/permission-bundles.
//
// Response: 201 created (or updated in place, same code) / 400 missing field / 404 role not found / 503 unavailable.
func (h *Handler) CreatePermissionBundle(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.BundleCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "bundle_code"})
		return
	}
	if len(req.PermittedActions) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "permitted_actions"})
		return
	}

	// The role's OWN tenant decides scope here, not anything the body
	// supplies (there is no tenant_id in this request at all) — before
	// this fix, any caller could grant a permission bundle onto any
	// tenant's role just by naming its role_id, since nothing checked
	// which tenant owns it. 404 rather than 403 so a probe against
	// another tenant's role_id cannot confirm it exists.
	role, err := h.store.FindRoleByID(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
			return
		}
		h.log.Error("CreatePermissionBundle: role lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if role.TenantID != tenantScope {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
		return
	}

	bundle, err := h.store.CreatePermissionBundle(r.Context(), domain.CreatePermissionBundleParams{
		RoleID: roleID, BundleCode: req.BundleCode, PermittedActions: req.PermittedActions,
	})
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
			return
		}
		h.log.Error("CreatePermissionBundle: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, bundle)
}

// ── POST /v1/admin/role-assignments ──────────────────────────────────────────

type createAssignmentRequest struct {
	PrincipalRoleAssignmentID string `json:"principal_role_assignment_id,omitempty"`
	PrincipalID               string `json:"principal_id"`
	RoleID                    string `json:"role_id"`
	// LegalEntityID is optional: omit it for a tenant-wide assignment
	// (only accepted if the role's scope_type is TENANT — see
	// domain.ErrLegalEntityRequiredForRoleScope).
	LegalEntityID string    `json:"legal_entity_id,omitempty"`
	EffectiveFrom time.Time `json:"effective_from"`
}

func (req createAssignmentRequest) missingField() string {
	switch {
	case req.PrincipalID == "":
		return "principal_id"
	case req.RoleID == "":
		return "role_id"
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	default:
		return ""
	}
}

// CreateRoleAssignment handles POST /v1/admin/role-assignments.
//
// Response: 201 created / 400 missing field / 404 role not found / 503 unavailable.
func (h *Handler) CreateRoleAssignment(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createAssignmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	// Same doctrine as CreatePermissionBundle: the role being assigned
	// decides the tenant, and it must be the caller's own — before this
	// fix, any caller could hand out a role from any tenant to any
	// principal, in any legal entity, just by naming role_id.
	role, err := h.store.FindRoleByID(r.Context(), req.RoleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
			return
		}
		h.log.Error("CreateRoleAssignment: role lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if role.TenantID != tenantScope {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
		return
	}

	var legalEntityID *string
	if req.LegalEntityID != "" {
		legalEntityID = &req.LegalEntityID
	}

	assignment, err := h.store.CreateRoleAssignment(r.Context(), domain.CreateRoleAssignmentParams{
		// assigned_by is always the verified caller, never the request body.
		PrincipalRoleAssignmentID: req.PrincipalRoleAssignmentID, PrincipalID: req.PrincipalID, RoleID: req.RoleID,
		LegalEntityID: legalEntityID, EffectiveFrom: req.EffectiveFrom, AssignedBy: principalID,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRoleNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
		case errors.Is(err, domain.ErrLegalEntityRequiredForRoleScope):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "legal_entity_id_required", "message": err.Error()})
		default:
			h.log.Error("CreateRoleAssignment: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, assignment)
}

// RevokeRoleAssignment handles POST /v1/admin/role-assignments/{assignment_id}/revoke.
//
// Response: 200 revoked / 404 not found or already ended / 503 unavailable.
func (h *Handler) RevokeRoleAssignment(w http.ResponseWriter, r *http.Request) {
	assignmentID := chi.URLParam(r, "assignment_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// The store's own query now carries the tenant predicate through the
	// assignment's role, so a cross-tenant revoke attempt reports
	// role_assignment_not_found rather than revoking (or even confirming
	// the existence of) another tenant's assignment.
	assignment, err := h.store.RevokeRoleAssignment(r.Context(), assignmentID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
			return
		}
		h.log.Error("RevokeRoleAssignment: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, assignment)
}

// ── POST /v1/admin/delegated-authorities ─────────────────────────────────────

type createDelegationRequest struct {
	DelegatedAuthorityID string `json:"delegated_authority_id,omitempty"`
	DelegatorPrincipalID string `json:"delegator_principal_id"`
	DelegatePrincipalID  string `json:"delegate_principal_id"`
	ScopeType            string `json:"scope_type"`
	// LegalEntityID is optional: omit it for a delegation that applies
	// across the whole tenant rather than one entity.
	LegalEntityID       string     `json:"legal_entity_id,omitempty"`
	AuthorityLimitType  *string    `json:"authority_limit_type,omitempty"`
	AuthorityLimitValue *string    `json:"authority_limit_value,omitempty"`
	EffectiveFrom       time.Time  `json:"effective_from"`
	EffectiveTo         *time.Time `json:"effective_to,omitempty"`
}

func (req createDelegationRequest) missingField() string {
	switch {
	case req.DelegatorPrincipalID == "":
		return "delegator_principal_id"
	case req.DelegatePrincipalID == "":
		return "delegate_principal_id"
	case req.ScopeType == "":
		return "scope_type"
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	default:
		return ""
	}
}

// CreateDelegatedAuthority handles POST /v1/admin/delegated-authorities.
//
// Response: 201 created / 400 missing field / 503 unavailable.
func (h *Handler) CreateDelegatedAuthority(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req createDelegationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	// delegated_authorities carries no tenant_id at all (only
	// legal_entity_id — see 000001's schema), so the one check this
	// service CAN make without inventing a column is the one that
	// matters most: this table had no ownership check whatsoever, so any
	// caller could delegate ANY principal's authority to ANY other
	// principal, purely by naming them in the body. A principal may only
	// give away authority that is theirs to give — the same doctrine
	// already enforced in delegated-authority-svc (a separate service
	// with its own copy of this concept — see docs/architecture/
	// full-architecture-gap-analysis.md item 12 on the duplication).
	if req.DelegatorPrincipalID != principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "delegator_must_be_caller",
			"message": "a principal may only delegate authority that is their own",
		})
		return
	}

	var legalEntityID *string
	if req.LegalEntityID != "" {
		legalEntityID = &req.LegalEntityID
	}

	d, err := h.store.CreateDelegatedAuthority(r.Context(), domain.CreateDelegatedAuthorityParams{
		DelegatedAuthorityID: req.DelegatedAuthorityID, DelegatorPrincipalID: req.DelegatorPrincipalID,
		DelegatePrincipalID: req.DelegatePrincipalID, ScopeType: req.ScopeType, LegalEntityID: legalEntityID,
		AuthorityLimitType: req.AuthorityLimitType, AuthorityLimitValue: req.AuthorityLimitValue,
		EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo,
	})
	if err != nil {
		h.log.Error("CreateDelegatedAuthority: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// RevokeDelegatedAuthority handles POST /v1/admin/delegated-authorities/{delegation_id}/revoke.
//
// Response: 200 revoked / 404 not found / 409 already revoked / 503 unavailable.
func (h *Handler) RevokeDelegatedAuthority(w http.ResponseWriter, r *http.Request) {
	delegationID := chi.URLParam(r, "delegation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Fetched and checked BEFORE revoking, same doctrine as
	// secret-vault-integration-svc's RevokeLease: only the delegator may
	// take back authority they gave away — this had no check of any kind
	// before, so any caller could revoke any principal's delegation.
	existing, err := h.store.FindDelegatedAuthorityByID(r.Context(), delegationID)
	if err != nil {
		if errors.Is(err, domain.ErrDelegatedAuthorityNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "delegated_authority_not_found"})
			return
		}
		h.log.Error("RevokeDelegatedAuthority: lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if existing.DelegatorPrincipalID != principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "only_delegator_may_revoke",
			"message": "only the principal who granted this delegation may revoke it",
		})
		return
	}

	d, err := h.store.RevokeDelegatedAuthority(r.Context(), delegationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDelegatedAuthorityNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "delegated_authority_not_found"})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already_revoked"})
		default:
			h.log.Error("RevokeDelegatedAuthority: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── POST /v1/admin/sod-rules ──────────────────────────────────────────────────

// ActionSoDRuleManageGlobal is the action a principal must hold, against the
// platform-scope legal entity, to author a segregation-of-duties rule that
// applies to every tenant. Deliberately not the same grant that authors a
// rule for one tenant: those are different blast radii.
const ActionSoDRuleManageGlobal = "SOD_RULE_MANAGE_GLOBAL"

type createSoDRuleRequest struct {
	DomainCode     string  `json:"domain_code"`
	ActionA        string  `json:"action_a"`
	ActionB        string  `json:"action_b"`
	ConflictType   string  `json:"conflict_type"`
	JurisdictionID *string `json:"jurisdiction_id,omitempty"`
	// TenantID is optional — omit for a rule that applies across every
	// tenant, matching JurisdictionID's own global-when-nil convention.
	TenantID *string `json:"tenant_id,omitempty"`
}

func (req createSoDRuleRequest) missingField() string {
	switch {
	case req.DomainCode == "":
		return "domain_code"
	case req.ActionA == "":
		return "action_a"
	case req.ActionB == "":
		return "action_b"
	case req.ConflictType == "":
		return "conflict_type"
	default:
		return ""
	}
}

// CreateSoDRule handles POST /v1/admin/sod-rules. If jurisdiction_id is
// supplied it's validated synchronously against jurisdiction-rules-svc,
// fail-closed.
//
// A rule with no tenant_id applies to EVERY tenant, and creating one
// therefore requires the platform-scope grant named by
// ActionSoDRuleManageGlobal — not the tenant scope that authorizes a rule for
// the caller's own tenant.
//
// Response: 201 created / 400 missing field / 401 missing principal or tenant
// scope / 403 not authorized for a platform-wide rule / 404 jurisdiction not
// found / 503 unavailable.
func (h *Handler) CreateSoDRule(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req createSoDRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	// A tenant scope is required to reach this route AT ALL, whether or not
	// the body names one. Requiring it only when tenant_id was present made
	// the omission the cheap path: with no tenant_id, one request carrying
	// nothing but a principal header stored a rule with tenant_id = NULL,
	// which CheckSoDConflict matches for every tenant. That is a platform-wide
	// denial of service delivered through the authorization engine — every
	// principal anywhere holding both actions gets DENIED on their next
	// /v1/authorize — and it crossed no tenant boundary because none was asked
	// for.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != nil && *req.TenantID != "" {
		if h.refuseForeignTenant(w, *req.TenantID, tenantScope) {
			return
		}
	} else {
		// nil tenant_id still means "applies to every tenant" — that is a
		// deliberate and useful thing to be able to say. It is now a distinct
		// grant to say it, rather than a side effect of leaving a field out.
		if !h.requirePlatformAction(w, r, principalID, ActionSoDRuleManageGlobal) {
			return
		}
	}

	if req.JurisdictionID != nil && *req.JurisdictionID != "" {
		if err := h.jurisdictionValidator.ValidateExists(r.Context(), *req.JurisdictionID); err != nil {
			switch {
			case errors.Is(err, domain.ErrJurisdictionNotFound):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "jurisdiction_not_found", "jurisdiction_id": *req.JurisdictionID})
			default:
				h.log.Error("CreateSoDRule: jurisdiction validation failed", zap.String("correlation_id", correlationID), zap.Error(err))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
			}
			return
		}
	}

	rule, err := h.store.CreateSoDRule(r.Context(), domain.CreateSoDRuleParams{
		DomainCode: req.DomainCode, ActionA: req.ActionA, ActionB: req.ActionB,
		ConflictType: req.ConflictType, JurisdictionID: req.JurisdictionID,
		TenantID: req.TenantID,
	})
	if err != nil {
		h.log.Error("CreateSoDRule: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// ── POST /v1/authorize ────────────────────────────────────────────────────────

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	// TenantID is the FALLBACK source of the tenant scope, kept only for
	// callers that do not forward X-Tenant-Id yet. See resolveTenantScope:
	// the header wins, and a body that disagrees with it is refused.
	TenantID string `json:"tenant_id,omitempty"`
}

type authorizeResponse struct {
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
}

// Authorize handles POST /v1/authorize — the core evaluation endpoint.
//
// Layers, in order:
//  1. RBAC — does the principal directly hold a role granting action_type
//     in legal_entity_id?
//  2. Delegated access — if not, does the principal have an active
//     delegation from someone who holds that grant?
//  3. SoD — if granted by either layer, does granting it conflict with
//     anything else the principal already holds (RBAC ∪ delegated)?
//
// Every evaluation — grant or deny — is written to access_decision_log
// before the response is returned (critical constraint: no material action
// without a decision artifact). On any internal error, the result is a
// denial, never a silent allow (fail-closed) — see the deferred-write
// comment below for the one exception, which is documented, not silent.
//
// ABAC is deliberately not implemented in v1 — no attribute-condition
// rules exist anywhere in the architecture docs to encode; see progress.md.
//
// The tenant scope of the evaluation comes from the verified X-Tenant-Id
// header when the caller sends one — see resolveTenantScope for why that
// matters more than it looks.
//
// Response: 200 with decision_outcome GRANTED|DENIED (both are 200 — the
// HTTP status reflects "the evaluation succeeded", not the outcome) /
// 400 missing field / 403 body tenant_id disagrees with the verified header /
// 503 store unavailable (fail-closed, no decision recorded).
// resolveTenantScope decides which tenant's SoD rules apply to this
// evaluation: the verified X-Tenant-Id header if the caller forwards one,
// otherwise the body's tenant_id, otherwise none.
//
// WHY THIS EXISTS. CheckSoDConflict's predicate is
// `tenant_id IS NULL OR tenant_id = <given>`, so an absent tenant narrows the
// check to globally-applicable rules ONLY. The tenant was read exclusively
// from the request body, and across this estate just three of roughly sixty
// authz clients put it there — so a tenant admin could create a
// segregation-of-duties rule, get a 201, see it in the register, and have it
// silently never applied to a decision made through any of the other
// fifty-odd services. SoD enforcement was opt-in per calling service, which
// is the inverse of what a control is for.
//
// Reading the header first makes the tenant arrive with the same identity the
// gateway already verified, so a caller that forwards the header cannot
// under-scope the check by omitting a body field. The body remains a fallback
// rather than an error because ~60 services call this endpoint and most send
// no tenant at all yet: rejecting them would replace a weak check with an
// outage. A body that CONTRADICTS the header is refused outright — that is
// not an old caller, it is a caller trying to be evaluated in someone else's
// scope.
func (h *Handler) resolveTenantScope(w http.ResponseWriter, r *http.Request, bodyTenantID string) (string, bool) {
	headerTenantID := r.Header.Get("X-Tenant-Id")

	if headerTenantID != "" {
		if bodyTenantID != "" && bodyTenantID != headerTenantID {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "tenant_scope_mismatch",
				"message": "request tenant_id does not match the caller's verified tenant scope",
			})
			return "", false
		}
		return headerTenantID, true
	}

	if bodyTenantID != "" {
		// Logged, not refused: this is the pre-header calling convention, and
		// the log is what makes the remaining callers findable.
		h.log.Debug("authorize: tenant scope taken from request body — caller does not forward X-Tenant-Id",
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")))
		return bodyTenantID, true
	}

	// No tenant anywhere: only globally-applicable SoD rules can be
	// considered. Warned at every call so "this tenant's SoD rules never
	// fired" is diagnosable from the logs rather than from an incident.
	h.log.Warn("authorize: no tenant scope supplied — only global SoD rules will be evaluated",
		zap.String("correlation_id", r.Header.Get("X-Correlation-ID")))
	return "", true
}

func (h *Handler) Authorize(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req authorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.PrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "principal_id"})
		return
	}
	if req.LegalEntityID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "legal_entity_id"})
		return
	}
	if req.ActionType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "action_type"})
		return
	}

	tenantScope, ok := h.resolveTenantScope(w, r, req.TenantID)
	if !ok {
		return
	}

	rbacActions, rbacBasis, err := h.store.FindGrantedActions(r.Context(), req.PrincipalID, req.LegalEntityID)
	if err != nil {
		// Fail-closed: the store is unreachable, so no decision can be made
		// or recorded. Returning 503 here (rather than a recorded DENIED)
		// is deliberate — the caller must treat "cannot evaluate" and
		// "evaluated and denied" as distinct outcomes, per the same
		// posture as every other service's ErrStoreUnavailable handling.
		h.log.Error("Authorize: store unavailable (rbac lookup)", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	granted := contains(rbacActions, req.ActionType)
	basis := rbacBasis
	allHeldActions := append([]string{}, rbacActions...)

	if !granted {
		delegatedActions, delegatedBasis, err := h.store.FindDelegatedActions(r.Context(), req.PrincipalID, req.LegalEntityID)
		if err != nil {
			h.log.Error("Authorize: store unavailable (delegation lookup)", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		allHeldActions = append(allHeldActions, delegatedActions...)
		if contains(delegatedActions, req.ActionType) {
			granted = true
			basis = delegatedBasis
		}
	}

	outcome := "DENIED"
	if !granted {
		basis = "no_grant"
	} else {
		// SoD check: does holding req.ActionType alongside anything else
		// this principal already holds violate a Separation-of-Duties rule?
		others := removeAll(allHeldActions, req.ActionType)
		conflicting, hasConflict, err := h.store.CheckSoDConflict(r.Context(), others, req.ActionType, tenantScope)
		if err != nil {
			h.log.Error("Authorize: store unavailable (sod check)", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if hasConflict {
			outcome = "DENIED"
			basis = "sod:conflict_with=" + conflicting
		} else {
			outcome = "GRANTED"
		}
	}

	decision, err := h.store.RecordAccessDecision(r.Context(), domain.RecordAccessDecisionParams{
		PrincipalID:   req.PrincipalID,
		LegalEntityID: req.LegalEntityID,
		ActionType:    req.ActionType,
		Outcome:       outcome,
		Basis:         basis,
		CorrelationID: correlationID,
		TenantID:      tenantScope,
	})
	if err != nil {
		h.log.Error("Authorize: failed to record access decision", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if outcome == "GRANTED" {
		if pubErr := h.publisher.PublishAuthorizationGranted(r.Context(), *decision); pubErr != nil {
			h.log.Error("Authorize: failed to publish authorization.granted", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
	} else {
		if pubErr := h.publisher.PublishAuthorizationDenied(r.Context(), *decision); pubErr != nil {
			h.log.Error("Authorize: failed to publish authorization.denied", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
		// Doc 05 §13.2 names "authorization grants/denials" as a required
		// SIEM signal. Only DENIED streams here — GRANTED is the overwhelming
		// majority outcome on this endpoint (it's called on nearly every
		// mutating request platform-wide), and streaming every success would
		// bury the actionable signal in noise rather than surface it.
		severity := siem.SeverityMedium
		isSoD := len(basis) > len("sod:conflict_with=") && basis[:len("sod:conflict_with=")] == "sod:conflict_with="
		if isSoD {
			severity = siem.SeverityHigh
		}
		h.siem.Stream(r.Context(), tenantScope, "authorization.denied", severity,
			"Authorization denied for principal "+req.PrincipalID+", action "+req.ActionType+": "+basis)
		if isSoD {
			conflictingAction := basis[len("sod:conflict_with="):]
			if pubErr := h.publisher.PublishSoDViolationDetected(r.Context(), *decision, conflictingAction); pubErr != nil {
				h.log.Error("Authorize: failed to publish sod.violation.detected", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		}
	}

	h.log.Info("authorization evaluated",
		zap.String("principal_id", req.PrincipalID),
		zap.String("action_type", req.ActionType),
		zap.String("outcome", outcome),
		zap.String("basis", basis),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, authorizeResponse{DecisionOutcome: outcome, DecisionBasis: basis, AccessDecisionID: decision.AccessDecisionID})
}

// ── GET /v1/access-decisions/{access_decision_id} ───────────────────────────

// GetAccessDecision handles GET /v1/access-decisions/{access_decision_id} —
// the "retrieve authorization rationale" capability.
//
// Authenticated and tenant-scoped. It was neither: the route checked no
// principal and no tenant, and the query carried no tenant or entity
// predicate, so anyone able to reach the port could walk decision ids and
// read principal_id, legal_entity_id, action_type, decision_outcome and
// decision_basis for every tenant — and decision_basis carries
// `sod:conflict_with=<action>`, which names where the segregation-of-duties
// tripwires are. A readable map of who may do what, and where the alarms are.
//
// A decision belonging to another tenant, and a decision recorded with no
// tenant at all, both answer 404 rather than 403 — a probe must not be able
// to confirm that an id exists.
//
// Response: 200 the decision / 401 missing principal or tenant scope /
// 404 not found, not yours, or unattributed / 503 unavailable.
func (h *Handler) GetAccessDecision(w http.ResponseWriter, r *http.Request) {
	accessDecisionID := chi.URLParam(r, "access_decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	d, err := h.store.FindAccessDecisionByID(r.Context(), accessDecisionID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDecisionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "access_decision_not_found"})
			return
		}
		h.log.Error("GetAccessDecision: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func contains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func removeAll(list []string, target string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
