// Package handler provides the HTTP handler for access-control-svc.
//
// API surface (all routes gated by this service's ownership boundary):
//
//	POST   /v1/roles                                       — CreateRole
//	GET    /v1/roles?tenant_id=                            — ListRoles
//	GET    /v1/roles/{role_id}                             — GetRole
//	POST   /v1/roles/{role_id}/deactivate                  — DeactivateRole
//	POST   /v1/permission-bundles                          — CreatePermissionBundle
//	GET    /v1/permission-bundles?tenant_id=               — ListBundles
//	GET    /v1/permission-bundles/{bundle_id}              — GetBundle
//	PUT    /v1/permission-bundles/{bundle_id}/actions      — UpdateBundleActions
//	POST   /v1/roles/{role_id}/permission-bundles/{bundle_id} — LinkBundle
//	DELETE /v1/roles/{role_id}/permission-bundles/{bundle_id} — UnlinkBundle
//	GET    /v1/roles/{role_id}/permission-bundles          — ListBundlesForRole
//
// Every mutating endpoint requires X-Principal-ID and X-Tenant-ID headers
// (injected by gateway-auth-svc from the identity envelope).
// No self-authorization — material write decisions route through
// authorization-svc per the governance doctrine.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
)

// AccessControlStore is the narrow store interface consumed by this handler.
type AccessControlStore interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	ListRolesByTenant(ctx context.Context, tenantID string) ([]domain.Role, error)
	DeactivateRole(ctx context.Context, roleID string) (*domain.Role, error)

	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error)
	FindBundleByID(ctx context.Context, bundleID string) (*domain.PermissionBundle, error)
	ListBundlesByTenant(ctx context.Context, tenantID string) ([]domain.PermissionBundle, error)
	UpdatePermissionBundleActions(ctx context.Context, params domain.UpdatePermissionBundleActionsParams) (*domain.PermissionBundle, error)

	CreateRolePermissionBundleLink(ctx context.Context, params domain.CreateRolePermissionBundleLinkParams) (*domain.RolePermissionBundleLink, error)
	RemoveRolePermissionBundleLink(ctx context.Context, params domain.RemoveRolePermissionBundleLinkParams) error
	ListBundlesForRole(ctx context.Context, roleID string) ([]domain.PermissionBundle, error)
}

// EventPublisher is the narrow event publisher interface consumed by this handler.
type EventPublisher interface {
	PublishRoleCreated(ctx context.Context, correlationID string, r domain.Role) error
	PublishRoleUpdated(ctx context.Context, correlationID string, r domain.Role) error
	PublishPermissionBundleUpdated(ctx context.Context, correlationID string, b domain.PermissionBundle) error
}

// Handler wires the store and event publisher to HTTP routes.
type Handler struct {
	store     AccessControlStore
	publisher EventPublisher
	log       *zap.Logger
}

// New constructs a Handler.
func New(store AccessControlStore, publisher EventPublisher, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, log: log}
}

// RegisterRoutes mounts all access-control-svc routes on the provided router.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	// Role management
	r.Post("/v1/roles", h.CreateRole)
	r.Get("/v1/roles", h.ListRoles)
	r.Get("/v1/roles/{role_id}", h.GetRole)
	r.Post("/v1/roles/{role_id}/deactivate", h.DeactivateRole)

	// Permission bundle management
	r.Post("/v1/permission-bundles", h.CreatePermissionBundle)
	r.Get("/v1/permission-bundles", h.ListBundles)
	r.Get("/v1/permission-bundles/{bundle_id}", h.GetBundle)
	r.Put("/v1/permission-bundles/{bundle_id}/actions", h.UpdateBundleActions)

	// Role-bundle link management
	r.Post("/v1/roles/{role_id}/permission-bundles/{bundle_id}", h.LinkBundle)
	r.Delete("/v1/roles/{role_id}/permission-bundles/{bundle_id}", h.UnlinkBundle)
	r.Get("/v1/roles/{role_id}/permission-bundles", h.ListBundlesForRole)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/roles ────────────────────────────────────────────────────────────

type createRoleRequest struct {
	RoleID               string `json:"role_id,omitempty"`
	TenantID             string `json:"tenant_id"`
	RoleCode             string `json:"role_code"`
	RoleName             string `json:"role_name"`
	RoleScopeType        string `json:"role_scope_type"`
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

// CreateRole handles POST /v1/roles. Idempotent on (tenant_id, role_code).
//
// Response: 201 created / 200 idempotent replay / 400 missing field /
// 409 conflict (same code, different name/scope) / 503 store unavailable.
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID := r.Header.Get("X-Principal-ID")

	var req createRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("invalid_json", err.Error()))
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, fieldErrResp(missing))
		return
	}

	role, created, err := h.store.CreateRole(r.Context(), domain.CreateRoleParams{
		RoleID:               req.RoleID,
		TenantID:             req.TenantID,
		RoleCode:             req.RoleCode,
		RoleName:             req.RoleName,
		RoleScopeType:        req.RoleScopeType,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeJSON(w, http.StatusConflict, errResp("role_conflict", "a role with this code exists with different name or scope"))
			return
		}
		h.log.Error("CreateRole: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if pubErr := h.publisher.PublishRoleCreated(r.Context(), correlationID, *role); pubErr != nil {
			h.log.Error("CreateRole: failed to publish role.created", zap.String("role_id", role.RoleID), zap.Error(pubErr))
		}
	}
	writeJSON(w, status, role)
}

// ── GET /v1/roles ─────────────────────────────────────────────────────────────

// ListRoles handles GET /v1/roles?tenant_id=<id>.
//
// Response: 200 array / 400 missing tenant_id / 503 unavailable.
func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, fieldErrResp("tenant_id"))
		return
	}

	roles, err := h.store.ListRolesByTenant(r.Context(), tenantID)
	if err != nil {
		h.log.Error("ListRoles: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}
	writeJSON(w, http.StatusOK, roles)
}

// ── GET /v1/roles/{role_id} ───────────────────────────────────────────────────

// GetRole handles GET /v1/roles/{role_id}.
//
// Response: 200 / 404 not found / 503 unavailable.
func (h *Handler) GetRole(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	role, err := h.store.FindRoleByID(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, errResp("role_not_found", ""))
			return
		}
		h.log.Error("GetRole: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// ── POST /v1/roles/{role_id}/deactivate ──────────────────────────────────────

// DeactivateRole handles POST /v1/roles/{role_id}/deactivate.
// Idempotent: if already deactivated returns the existing record with 200.
//
// Response: 200 / 404 not found / 503 unavailable.
func (h *Handler) DeactivateRole(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	role, err := h.store.DeactivateRole(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, errResp("role_not_found", ""))
			return
		}
		h.log.Error("DeactivateRole: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}

	if pubErr := h.publisher.PublishRoleUpdated(r.Context(), correlationID, *role); pubErr != nil {
		h.log.Error("DeactivateRole: failed to publish role.updated", zap.String("role_id", role.RoleID), zap.Error(pubErr))
	}
	writeJSON(w, http.StatusOK, role)
}

// ── POST /v1/permission-bundles ───────────────────────────────────────────────

type createBundleRequest struct {
	PermissionBundleID string   `json:"permission_bundle_id,omitempty"`
	TenantID           string   `json:"tenant_id"`
	BundleCode         string   `json:"bundle_code"`
	BundleName         string   `json:"bundle_name"`
	PermittedActions   []string `json:"permitted_actions"`
}

func (req createBundleRequest) missingField() string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.BundleCode == "":
		return "bundle_code"
	case req.BundleName == "":
		return "bundle_name"
	case len(req.PermittedActions) == 0:
		return "permitted_actions"
	default:
		return ""
	}
}

// CreatePermissionBundle handles POST /v1/permission-bundles.
// Idempotent on (tenant_id, bundle_code) when bundle_name matches.
//
// Response: 201 created / 200 idempotent replay / 400 / 409 / 503.
func (h *Handler) CreatePermissionBundle(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("invalid_json", err.Error()))
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, fieldErrResp(missing))
		return
	}

	bundle, created, err := h.store.CreatePermissionBundle(r.Context(), domain.CreatePermissionBundleParams{
		PermissionBundleID: req.PermissionBundleID,
		TenantID:           req.TenantID,
		BundleCode:         req.BundleCode,
		BundleName:         req.BundleName,
		PermittedActions:   req.PermittedActions,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeJSON(w, http.StatusConflict, errResp("bundle_conflict", "a bundle with this code exists with a different name"))
			return
		}
		h.log.Error("CreatePermissionBundle: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, bundle)
}

// ── GET /v1/permission-bundles ────────────────────────────────────────────────

// ListBundles handles GET /v1/permission-bundles?tenant_id=<id>.
func (h *Handler) ListBundles(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, fieldErrResp("tenant_id"))
		return
	}

	bundles, err := h.store.ListBundlesByTenant(r.Context(), tenantID)
	if err != nil {
		h.log.Error("ListBundles: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}
	writeJSON(w, http.StatusOK, bundles)
}

// ── GET /v1/permission-bundles/{bundle_id} ────────────────────────────────────

// GetBundle handles GET /v1/permission-bundles/{bundle_id}.
func (h *Handler) GetBundle(w http.ResponseWriter, r *http.Request) {
	bundleID := chi.URLParam(r, "bundle_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	bundle, err := h.store.FindBundleByID(r.Context(), bundleID)
	if err != nil {
		if errors.Is(err, domain.ErrBundleNotFound) {
			writeJSON(w, http.StatusNotFound, errResp("bundle_not_found", ""))
			return
		}
		h.log.Error("GetBundle: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

// ── PUT /v1/permission-bundles/{bundle_id}/actions ────────────────────────────

type updateActionsRequest struct {
	PermittedActions []string `json:"permitted_actions"`
}

// UpdateBundleActions handles PUT /v1/permission-bundles/{bundle_id}/actions.
// Atomically replaces the permitted_actions array of an existing bundle.
//
// Response: 200 / 400 / 404 / 503.
func (h *Handler) UpdateBundleActions(w http.ResponseWriter, r *http.Request) {
	bundleID := chi.URLParam(r, "bundle_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req updateActionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("invalid_json", err.Error()))
		return
	}
	if len(req.PermittedActions) == 0 {
		writeJSON(w, http.StatusBadRequest, fieldErrResp("permitted_actions"))
		return
	}

	bundle, err := h.store.UpdatePermissionBundleActions(r.Context(), domain.UpdatePermissionBundleActionsParams{
		PermissionBundleID: bundleID,
		PermittedActions:   req.PermittedActions,
	})
	if err != nil {
		if errors.Is(err, domain.ErrBundleNotFound) {
			writeJSON(w, http.StatusNotFound, errResp("bundle_not_found", ""))
			return
		}
		h.log.Error("UpdateBundleActions: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}

	if pubErr := h.publisher.PublishPermissionBundleUpdated(r.Context(), correlationID, *bundle); pubErr != nil {
		h.log.Error("UpdateBundleActions: failed to publish permission.bundle.updated", zap.String("bundle_id", bundle.PermissionBundleID), zap.Error(pubErr))
	}
	writeJSON(w, http.StatusOK, bundle)
}

// ── POST /v1/roles/{role_id}/permission-bundles/{bundle_id} ──────────────────

// LinkBundle handles POST /v1/roles/{role_id}/permission-bundles/{bundle_id}.
// Links an existing bundle to a role. Idempotent.
//
// Response: 200 / 404 role or bundle not found / 422 role deactivated / 503.
func (h *Handler) LinkBundle(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	bundleID := chi.URLParam(r, "bundle_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID := r.Header.Get("X-Principal-ID")

	link, err := h.store.CreateRolePermissionBundleLink(r.Context(), domain.CreateRolePermissionBundleLinkParams{
		RoleID:             roleID,
		PermissionBundleID: bundleID,
		CreatedBy:          principalID,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRoleNotFound):
			writeJSON(w, http.StatusNotFound, errResp("role_not_found", ""))
		case errors.Is(err, domain.ErrBundleNotFound):
			writeJSON(w, http.StatusNotFound, errResp("bundle_not_found", ""))
		case errors.Is(err, domain.ErrRoleDeactivated):
			writeJSON(w, http.StatusUnprocessableEntity, errResp("role_deactivated", "cannot link bundles to a deactivated role"))
		default:
			h.log.Error("LinkBundle: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		}
		return
	}

	// Emit role.updated so authorization-svc can re-evaluate effective grants.
	if role, lookupErr := h.store.FindRoleByID(r.Context(), roleID); lookupErr == nil {
		if pubErr := h.publisher.PublishRoleUpdated(r.Context(), correlationID, *role); pubErr != nil {
			h.log.Error("LinkBundle: failed to publish role.updated", zap.String("role_id", roleID), zap.Error(pubErr))
		}
	}
	writeJSON(w, http.StatusOK, link)
}

// ── DELETE /v1/roles/{role_id}/permission-bundles/{bundle_id} ────────────────

// UnlinkBundle handles DELETE /v1/roles/{role_id}/permission-bundles/{bundle_id}.
// Deactivates (not deletes) the role-bundle link.
//
// Response: 204 / 404 link not found / 503 unavailable.
func (h *Handler) UnlinkBundle(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	bundleID := chi.URLParam(r, "bundle_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	err := h.store.RemoveRolePermissionBundleLink(r.Context(), domain.RemoveRolePermissionBundleLinkParams{
		RoleID:             roleID,
		PermissionBundleID: bundleID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrLinkNotFound) {
			writeJSON(w, http.StatusNotFound, errResp("link_not_found", "role-bundle link not found or already removed"))
			return
		}
		h.log.Error("UnlinkBundle: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}

	// Emit role.updated so authorization-svc can re-evaluate effective grants.
	if role, lookupErr := h.store.FindRoleByID(r.Context(), roleID); lookupErr == nil {
		if pubErr := h.publisher.PublishRoleUpdated(r.Context(), correlationID, *role); pubErr != nil {
			h.log.Error("UnlinkBundle: failed to publish role.updated", zap.String("role_id", roleID), zap.Error(pubErr))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── GET /v1/roles/{role_id}/permission-bundles ────────────────────────────────

// ListBundlesForRole handles GET /v1/roles/{role_id}/permission-bundles.
//
// Response: 200 array / 404 role not found / 503 unavailable.
func (h *Handler) ListBundlesForRole(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, err := h.store.FindRoleByID(r.Context(), roleID); err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, errResp("role_not_found", ""))
			return
		}
		h.log.Error("ListBundlesForRole: role lookup unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}

	bundles, err := h.store.ListBundlesForRole(r.Context(), roleID)
	if err != nil {
		h.log.Error("ListBundlesForRole: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, errResp("store_unavailable", ""))
		return
	}
	writeJSON(w, http.StatusOK, bundles)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}

func errResp(code, msg string) map[string]string {
	m := map[string]string{"error": code}
	if msg != "" {
		m["message"] = msg
	}
	return m
}

func fieldErrResp(field string) map[string]string {
	return map[string]string{"error": "missing_field", "field": field}
}
