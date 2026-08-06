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

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
)

type Store interface {
	CreateRole(ctx context.Context, r *domain.RoleDefinition) (created bool, err error)
	GetRole(ctx context.Context, roleDefinitionID string) (*domain.RoleDefinition, error)
	ListRoles(ctx context.Context, status string) ([]domain.RoleDefinition, error)
	UpdateRole(ctx context.Context, roleDefinitionID, roleName, status string) (*domain.RoleDefinition, error)

	CreateBundle(ctx context.Context, b *domain.PermissionBundleDef) (created bool, err error)
	ListBundles(ctx context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error)
}

type Publisher interface {
	PublishRoleCreated(ctx context.Context, r domain.RoleDefinition)
	PublishRoleUpdated(ctx context.Context, r domain.RoleDefinition)
	PublishBundleUpdated(ctx context.Context, b domain.PermissionBundleDef)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// AuthzAdmin provisions the role/bundle definition into authorization-svc's
// real admin API — the step that makes a definition actually enforced.
type AuthzAdmin interface {
	CreateRole(ctx context.Context, roleID, tenantID, roleCode, roleName, roleScopeType, createdByPrincipalID string) error
	CreatePermissionBundle(ctx context.Context, roleID, bundleCode string, permittedActions []string) error
}

const (
	actionRoleManage = "ACCESS_ROLE_MANAGE"
	actionRoleView   = "ACCESS_ROLE_VIEW"
)

type Handler struct {
	store      Store
	publisher  Publisher
	authz      AuthZClient
	authzAdmin AuthzAdmin
	log        *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, authzAdmin AuthzAdmin, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, authzAdmin: authzAdmin, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/role-definitions", func(r chi.Router) {
		r.Post("/", h.CreateRole)
		r.Get("/", h.ListRoles)
		r.Get("/{role_definition_id}", h.GetRole)
		r.Patch("/{role_definition_id}", h.UpdateRole)
		r.Post("/{role_definition_id}/permission-bundles", h.CreateBundle)
		r.Get("/{role_definition_id}/permission-bundles", h.ListBundles)
	})
}

// ── POST /v1/role-definitions ─────────────────────────────────────────────────

// CreateRole records the role definition here AND provisions it for real
// enforcement via a synchronous call to authorization-svc's admin API. A
// role is never recorded as created here without also having been actually
// provisioned — if the admin API call fails, nothing is persisted.
//
// Idempotent on (tenant_id, correlation_id).
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.RoleCode == "" || req.RoleName == "" || req.RoleScopeType == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, role_code, role_name, role_scope_type, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	roleID := uuid.NewString()

	if err := h.authzAdmin.CreateRole(r.Context(), roleID, tenantID, req.RoleCode, req.RoleName, req.RoleScopeType, principalID); err != nil {
		h.log.Error("failed to provision role in authorization-svc", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	role := &domain.RoleDefinition{
		RoleDefinitionID:     roleID,
		TenantID:             tenantID,
		RoleCode:             req.RoleCode,
		RoleName:             req.RoleName,
		RoleScopeType:        req.RoleScopeType,
		Status:               domain.RoleStatusActive,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	created, err := h.store.CreateRole(r.Context(), role)
	if err != nil {
		h.log.Error("failed to record role definition", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if created {
		h.publisher.PublishRoleCreated(r.Context(), *role)
	}

	writeJSON(w, http.StatusCreated, role)
}

// ── GET /v1/role-definitions ──────────────────────────────────────────────────

func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListRoles(r.Context(), status)
	if err != nil {
		h.log.Error("failed to list role definitions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.RoleDefinition{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/role-definitions/{role_definition_id} ─────────────────────────────

func (h *Handler) GetRole(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	role, err := h.store.GetRole(r.Context(), roleDefinitionID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// ── PATCH /v1/role-definitions/{role_definition_id} ───────────────────────────

func (h *Handler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	var req domain.UpdateRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.UpdateRole(r.Context(), roleDefinitionID, req.RoleName, req.Status)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	h.publisher.PublishRoleUpdated(r.Context(), *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/role-definitions/{role_definition_id}/permission-bundles ────────

// CreateBundle records the bundle here AND provisions it into
// authorization-svc via the admin API, attached to the role provisioned at
// role-creation time.
//
// Idempotent on (tenant_id, correlation_id).
func (h *Handler) CreateBundle(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	var req domain.CreateBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.BundleCode == "" || len(req.PermittedActions) == 0 || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "bundle_code, permitted_actions, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if _, err := h.store.GetRole(r.Context(), roleDefinitionID); err != nil {
		h.writeStoreErr(w, err)
		return
	}

	if err := h.authzAdmin.CreatePermissionBundle(r.Context(), roleDefinitionID, req.BundleCode, req.PermittedActions); err != nil {
		h.log.Error("failed to provision permission bundle in authorization-svc", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	bundle := &domain.PermissionBundleDef{
		BundleID:         uuid.NewString(),
		TenantID:         svcmiddleware.TenantFromContext(r.Context()),
		RoleDefinitionID: roleDefinitionID,
		BundleCode:       req.BundleCode,
		PermittedActions: req.PermittedActions,
		ActiveFlag:       true,
		CorrelationID:    req.CorrelationID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	created, err := h.store.CreateBundle(r.Context(), bundle)
	if err != nil {
		h.log.Error("failed to record permission bundle", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if created {
		h.publisher.PublishBundleUpdated(r.Context(), *bundle)
	}

	writeJSON(w, http.StatusCreated, bundle)
}

// ── GET /v1/role-definitions/{role_definition_id}/permission-bundles ─────────

func (h *Handler) ListBundles(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListBundles(r.Context(), roleDefinitionID)
	if err != nil {
		h.log.Error("failed to list permission bundles", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.PermissionBundleDef{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrRoleNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	h.log.Error("access control store error", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
