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
)

// AuthorizationStore is the narrow interface the handler depends on.
type AuthorizationStore interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error)
	CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID string) (*domain.PrincipalRoleAssignment, error)
	CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error)
	RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID string) (*domain.DelegatedAuthority, error)
	CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error)
	FindGrantedActions(ctx context.Context, principalID, legalEntityID string) ([]string, string, error)
	FindDelegatedActions(ctx context.Context, principalID, legalEntityID string) ([]string, string, error)
	CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction string) (string, bool, error)
	RecordAccessDecision(ctx context.Context, principalID, legalEntityID, actionType, outcome, basis, correlationID string) (*domain.AccessDecisionLog, error)
	FindAccessDecisionByID(ctx context.Context, accessDecisionID string) (*domain.AccessDecisionLog, error)
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
	log                   *zap.Logger
}

func New(store AuthorizationStore, publisher EventPublisher, jurisdictionValidator jurisdiction.Validator, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, jurisdictionValidator: jurisdictionValidator, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/admin/roles", h.CreateRole)
	r.Post("/v1/admin/roles/{role_id}/permission-bundles", h.CreatePermissionBundle)
	r.Post("/v1/admin/role-assignments", h.CreateRoleAssignment)
	r.Post("/v1/admin/role-assignments/{assignment_id}/revoke", h.RevokeRoleAssignment)
	r.Post("/v1/admin/delegated-authorities", h.CreateDelegatedAuthority)
	r.Post("/v1/admin/delegated-authorities/{delegation_id}/revoke", h.RevokeDelegatedAuthority)
	r.Post("/v1/admin/sod-rules", h.CreateSoDRule)

	r.Post("/v1/authorize", h.Authorize)
	r.Get("/v1/access-decisions/{access_decision_id}", h.GetAccessDecision)
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
	RoleID               string `json:"role_id,omitempty"`
	TenantID             string `json:"tenant_id"`
	RoleCode             string `json:"role_code"`
	RoleName             string `json:"role_name"`
	RoleScopeType        string `json:"role_scope_type"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
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
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// CreateRole handles POST /v1/admin/roles. Idempotent on (tenant_id, role_code).
//
// Response: 201 created / 200 idempotent replay / 400 missing field / 409 conflict / 503 unavailable.
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	role, created, err := h.store.CreateRole(r.Context(), domain.CreateRoleParams{
		RoleID: req.RoleID, TenantID: req.TenantID, RoleCode: req.RoleCode,
		RoleName: req.RoleName, RoleScopeType: req.RoleScopeType, CreatedByPrincipalID: req.CreatedByPrincipalID,
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
	PrincipalRoleAssignmentID string    `json:"principal_role_assignment_id,omitempty"`
	PrincipalID               string    `json:"principal_id"`
	RoleID                    string    `json:"role_id"`
	LegalEntityID             string    `json:"legal_entity_id"`
	EffectiveFrom             time.Time `json:"effective_from"`
	AssignedBy                string    `json:"assigned_by"`
}

func (req createAssignmentRequest) missingField() string {
	switch {
	case req.PrincipalID == "":
		return "principal_id"
	case req.RoleID == "":
		return "role_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	case req.AssignedBy == "":
		return "assigned_by"
	default:
		return ""
	}
}

// CreateRoleAssignment handles POST /v1/admin/role-assignments.
//
// Response: 201 created / 400 missing field / 404 role not found / 503 unavailable.
func (h *Handler) CreateRoleAssignment(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createAssignmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	assignment, err := h.store.CreateRoleAssignment(r.Context(), domain.CreateRoleAssignmentParams{
		PrincipalRoleAssignmentID: req.PrincipalRoleAssignmentID, PrincipalID: req.PrincipalID, RoleID: req.RoleID,
		LegalEntityID: req.LegalEntityID, EffectiveFrom: req.EffectiveFrom, AssignedBy: req.AssignedBy,
	})
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
			return
		}
		h.log.Error("CreateRoleAssignment: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
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

	assignment, err := h.store.RevokeRoleAssignment(r.Context(), assignmentID)
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
	DelegatedAuthorityID string     `json:"delegated_authority_id,omitempty"`
	DelegatorPrincipalID string     `json:"delegator_principal_id"`
	DelegatePrincipalID  string     `json:"delegate_principal_id"`
	ScopeType            string     `json:"scope_type"`
	LegalEntityID        string     `json:"legal_entity_id"`
	AuthorityLimitType   *string    `json:"authority_limit_type,omitempty"`
	AuthorityLimitValue  *string    `json:"authority_limit_value,omitempty"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
}

func (req createDelegationRequest) missingField() string {
	switch {
	case req.DelegatorPrincipalID == "":
		return "delegator_principal_id"
	case req.DelegatePrincipalID == "":
		return "delegate_principal_id"
	case req.ScopeType == "":
		return "scope_type"
	case req.LegalEntityID == "":
		return "legal_entity_id"
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

	var req createDelegationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	d, err := h.store.CreateDelegatedAuthority(r.Context(), domain.CreateDelegatedAuthorityParams{
		DelegatedAuthorityID: req.DelegatedAuthorityID, DelegatorPrincipalID: req.DelegatorPrincipalID,
		DelegatePrincipalID: req.DelegatePrincipalID, ScopeType: req.ScopeType, LegalEntityID: req.LegalEntityID,
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

type createSoDRuleRequest struct {
	DomainCode     string  `json:"domain_code"`
	ActionA        string  `json:"action_a"`
	ActionB        string  `json:"action_b"`
	ConflictType   string  `json:"conflict_type"`
	JurisdictionID *string `json:"jurisdiction_id,omitempty"`
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
// Response: 201 created / 400 missing field / 404 jurisdiction not found / 503 unavailable.
func (h *Handler) CreateSoDRule(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createSoDRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
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
// Response: 200 with decision_outcome GRANTED|DENIED (both are 200 — the
// HTTP status reflects "the evaluation succeeded", not the outcome) /
// 400 missing field / 503 store unavailable (fail-closed, no decision recorded).
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
		conflicting, hasConflict, err := h.store.CheckSoDConflict(r.Context(), others, req.ActionType)
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

	decision, err := h.store.RecordAccessDecision(r.Context(), req.PrincipalID, req.LegalEntityID, req.ActionType, outcome, basis, correlationID)
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
		if len(basis) > len("sod:conflict_with=") && basis[:len("sod:conflict_with=")] == "sod:conflict_with=" {
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
// Response: 200 the decision / 404 not found / 503 unavailable.
func (h *Handler) GetAccessDecision(w http.ResponseWriter, r *http.Request) {
	accessDecisionID := chi.URLParam(r, "access_decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	d, err := h.store.FindAccessDecisionByID(r.Context(), accessDecisionID)
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
