package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
)

type Store interface {
	CreateRole(ctx context.Context, r *domain.RoleDefinition) (created bool, err error)
	GetRole(ctx context.Context, roleDefinitionID string) (*domain.RoleDefinition, error)
	ListRoles(ctx context.Context, filter domain.ListFilter) ([]domain.RoleDefinition, error)
	UpdateRole(ctx context.Context, roleDefinitionID, roleName, status, updatedByPrincipalID string) (*domain.RoleDefinition, error)

	CreateBundle(ctx context.Context, b *domain.PermissionBundleDef) (created bool, err error)
	ListBundles(ctx context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error)
	GetBundle(ctx context.Context, roleDefinitionID, bundleID string) (*domain.PermissionBundleDef, error)
	UpdateBundle(ctx context.Context, roleDefinitionID, bundleID string, permittedActions []string, activeFlag *bool, updatedByPrincipalID string) (*domain.PermissionBundleDef, error)
	ListAllBundles(ctx context.Context, filter domain.BundleListFilter) ([]domain.PermissionBundleDef, error)
}

type Publisher interface {
	PublishRoleCreated(ctx context.Context, r domain.RoleDefinition, actorID string)
	PublishRoleUpdated(ctx context.Context, r domain.RoleDefinition, actorID string)
	PublishBundleUpdated(ctx context.Context, b domain.PermissionBundleDef, actorID string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// AuthzAdmin provisions the role/bundle definition into authorization-svc's
// real admin API — the step that makes a definition actually enforced.
// Every method takes a clients.Scope rather than a tail of bare strings. Two
// of the three used to be called with empty principal and tenant, which
// authorization-svc's admin routes reject outright — a struct turns that
// omission into something the compiler catches.
type AuthzAdmin interface {
	CreateRole(ctx context.Context, roleID, roleCode, roleName, roleScopeType string, s clients.Scope) error
	CreatePermissionBundle(ctx context.Context, roleID, bundleCode string, permittedActions []string, s clients.Scope) error
	SetRoleActive(ctx context.Context, roleID string, active bool, s clients.Scope) error
	// SetPermissionBundleActive retires or reactivates the bundle with
	// bundleCode by resolving it against authorization-svc's own id space.
	SetPermissionBundleActive(ctx context.Context, roleID, bundleCode string, active bool, s clients.Scope) error
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
		r.Get("/{role_definition_id}/permission-bundles/{bundle_id}", h.GetBundle)
		r.Patch("/{role_definition_id}/permission-bundles/{bundle_id}", h.UpdateBundle)
		r.Delete("/{role_definition_id}/permission-bundles/{bundle_id}", h.DetachBundle)
	})
	// The flat catalogue read across roles — the one register listing the
	// role-scoped routes cannot provide. Like everything else it is scoped by
	// the verified X-Tenant-Id, so this is a slice of this tenant's catalogue,
	// not a platform-wide dump.
	r.Route("/v1/permission-bundles", func(r chi.Router) {
		r.Get("/", h.ListAllBundles)
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	roleID := uuid.NewString()

	authzScope := clients.Scope{
		PrincipalID:   principalID,
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		CorrelationID: req.CorrelationID,
	}
	if err := h.authzAdmin.CreateRole(r.Context(), roleID, req.RoleCode, req.RoleName, req.RoleScopeType, authzScope); err != nil {
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
		h.publisher.PublishRoleCreated(r.Context(), *role, principalID)
	}

	writeJSON(w, http.StatusCreated, role)
}

// ── GET /v1/role-definitions ──────────────────────────────────────────────────

// ListRoles reads the role catalogue, optionally narrowed by status,
// scope_type, a code/name search, and limit/offset paging. Zero limit means
// no cap: paging is something a caller asks for, because a silently truncated
// catalogue would make its totals wrong.
func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.ListFilter{Query: r.URL.Query().Get("search")}

	status := r.URL.Query().Get("status")
	if status != "" {
		if status != string(domain.RoleStatusActive) && status != string(domain.RoleStatusRetired) {
			writeError(w, http.StatusBadRequest, "invalid_status",
				fmt.Sprintf("status must be %s or %s, got %q", domain.RoleStatusActive, domain.RoleStatusRetired, status))
			return
		}
		filter.Status = status
	}
	scopeType := r.URL.Query().Get("scope_type")
	if scopeType != "" {
		if scopeType != "LEGAL_ENTITY" && scopeType != "TENANT" {
			writeError(w, http.StatusBadRequest, "invalid_scope_type", "scope_type must be LEGAL_ENTITY or TENANT")
			return
		}
		filter.ScopeType = scopeType
	}
	limit, err := parseIntQuery(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	offset, err := parseIntQuery(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_offset", err.Error())
		return
	}
	filter.Limit, filter.Offset = limit, offset

	list, err := h.store.ListRoles(r.Context(), filter)
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
	_, ok = h.requireTenant(w, r)
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
	updateTenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Reject an unknown status instead of storing it.
	//
	// This used to pass req.Status straight to the store, and the column is a
	// bare VARCHAR(20) with the vocabulary only in a comment -- so
	// {"status":"BANANA"} persisted, and ListRoles(?status=ACTIVE) then omitted
	// a role that was neither active nor retired but simply unreadable. The
	// CHECK constraint added in 000002 is the backstop; this is the error the
	// caller can act on.
	if req.Status != "" && req.Status != string(domain.RoleStatusActive) && req.Status != string(domain.RoleStatusRetired) {
		writeError(w, http.StatusBadRequest, "invalid_status",
			fmt.Sprintf("status must be %s or %s, got %q", domain.RoleStatusActive, domain.RoleStatusRetired, req.Status))
		return
	}

	// PROPAGATE THE STATUS CHANGE BEFORE RECORDING IT, and fail closed.
	//
	// This is the same ordering CreateRole uses, for the same reason: the
	// catalogue must never claim a state the platform is not actually
	// enforcing. Retiring here without telling authorization-svc left the role
	// fully live -- FindGrantedActions joins through roles.active_flag, so
	// every principal holding it kept every action, while this service's
	// register displayed RETIRED. A governance record that disagrees with the
	// enforcement it describes is worse than no record.
	//
	// Only a real transition is propagated. An empty status means "rename
	// only", and PATCHing the status it already has is a no-op here rather than
	// a redundant call -- though the remote is idempotent either way.
	if req.Status != "" {
		current, err := h.store.GetRole(r.Context(), roleDefinitionID)
		if err != nil {
			h.writeStoreErr(w, err)
			return
		}
		if req.Status != string(current.Status) {
			active := req.Status == string(domain.RoleStatusActive)
			// req.LegalEntityID, not the stored definition: RoleDefinition does
			// not carry an entity, and this is the same value the CheckAllowed
			// above authorized against — so the propagated call is scoped to
			// exactly the entity the caller was permitted on.
			if err := h.authzAdmin.SetRoleActive(r.Context(), roleDefinitionID, active, clients.Scope{
				PrincipalID:   principalID,
				TenantID:      updateTenantID,
				LegalEntityID: req.LegalEntityID,
				CorrelationID: req.CorrelationID,
			}); err != nil {
				h.log.Error("failed to propagate role status to authorization-svc",
					zap.String("role_definition_id", roleDefinitionID),
					zap.String("status", req.Status),
					zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
				return
			}
		}
	}

	updated, err := h.store.UpdateRole(r.Context(), roleDefinitionID, req.RoleName, req.Status, principalID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	h.publisher.PublishRoleUpdated(r.Context(), *updated, principalID)
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
	bundleTenantID, ok := h.requireTenant(w, r)
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

	if err := h.authzAdmin.CreatePermissionBundle(r.Context(), roleDefinitionID, req.BundleCode, req.PermittedActions, clients.Scope{
		PrincipalID:   principalID,
		TenantID:      bundleTenantID,
		LegalEntityID: req.LegalEntityID,
		CorrelationID: req.CorrelationID,
	}); err != nil {
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
		h.publisher.PublishBundleUpdated(r.Context(), *bundle, principalID)
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
	_, ok = h.requireTenant(w, r)
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

// ── GET /v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id} ──

func (h *Handler) GetBundle(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")
	bundleID := chi.URLParam(r, "bundle_id")

	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	bundle, err := h.store.GetBundle(r.Context(), roleDefinitionID, bundleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

// ── PATCH /v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id} ──

// UpdateBundle edits ONE bundle's permitted actions and/or active state.
// Both are propagated to authorization-svc BEFORE they are recorded, and the
// request fails closed if the propagation cannot happen — the same ordering
// and refusal posture as CreateRole, CreateBundle and UpdateRole, for the
// same reason: the register must never claim a state the platform is not
// enforcing.
//
// An action edit is propagated as an upsert-replace of the bundle
// (authorization-svc's POST .../permission-bundles is idempotent on
// (role_id, bundle_code)), which is why re-attaching a bundle with the same
// code edits rather than duplicates. An active-state change is propagated via
// the retire/reactivate endpoints. Exactly the real transitions are propagated:
// a no-op replay returns the current bundle unchanged and makes no remote call.
//
// A bundle_code is intentionally NOT editable here. authorization-svc's admin
// API has no rename, so a rename here could only be a local lie about what the
// evaluation plane enforces. Detaching and re-attaching under a new code is the
// supported way to rename.
func (h *Handler) UpdateBundle(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")
	bundleID := chi.URLParam(r, "bundle_id")

	var req domain.UpdateBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = uuid.NewString()
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// A partial update that changes nothing would be a no-op write; refuse it
	// rather than silently accept a request whose intent cannot be read. Both
	// fields absent is far more likely a malformed client than a deliberate
	// replay — a genuine replay sends the same values and is handled below.
	if req.PermittedActions == nil && req.ActiveFlag == nil {
		writeError(w, http.StatusBadRequest, "nothing_to_update", "supply permitted_actions and/or active_flag")
		return
	}
	if req.PermittedActions != nil && len(req.PermittedActions) == 0 {
		writeError(w, http.StatusBadRequest, "empty_actions", "a bundle must permit at least one action; set active_flag to withdraw the bundle instead")
		return
	}

	current, err := h.store.GetBundle(r.Context(), roleDefinitionID, bundleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	actionsChanged := req.PermittedActions != nil && !slices.Equal(current.PermittedActions, req.PermittedActions)
	activeChanged := req.ActiveFlag != nil && *req.ActiveFlag != current.ActiveFlag
	if !actionsChanged && !activeChanged {
		writeJSON(w, http.StatusOK, current)
		return
	}

	scope := clients.Scope{
		PrincipalID:   principalID,
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		CorrelationID: req.CorrelationID,
	}
	if actionsChanged {
		// Upsert-replace into authorization-svc: idempotent on
		// (role_id, bundle_code), so this is an edit rather than a duplicate.
		if err := h.authzAdmin.CreatePermissionBundle(r.Context(), roleDefinitionID, current.BundleCode, req.PermittedActions, scope); err != nil {
			h.log.Error("failed to propagate permission bundle actions to authorization-svc",
				zap.String("bundle_id", bundleID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
			return
		}
	}
	if activeChanged {
		if err := h.authzAdmin.SetPermissionBundleActive(r.Context(), roleDefinitionID, current.BundleCode, *req.ActiveFlag, scope); err != nil {
			h.log.Error("failed to propagate permission bundle active state to authorization-svc",
				zap.String("bundle_id", bundleID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
			return
		}
	}

	updated, err := h.store.UpdateBundle(r.Context(), roleDefinitionID, bundleID, req.PermittedActions, req.ActiveFlag, principalID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	h.publisher.PublishBundleUpdated(r.Context(), *updated, principalID)
	writeJSON(w, http.StatusOK, updated)
}

// ── DELETE /v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id} ──

// DetachBundle withdraws ONE bundle from its role: sets active_flag false here
// AND retires it in authorization-svc by resolving its code to the remote
// id. Fail-closed and idempotent: already-detached 200s with the current
// record, and a retirement that cannot be propagated refuses the request, so a
// detached row here always means a detached bundle there.
//
// No hard-delete, matching authorization-svc's own retire posture: a grant
// recorded as `rbac:role=<code>` is only explainable while the actions that
// role once held remain readable.
func (h *Handler) DetachBundle(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")
	bundleID := chi.URLParam(r, "bundle_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// DELETE carries no body over this API surface; the request context rides
	// the query string instead (legal_entity_id, correlation_id). A body is
	// still honoured if present, for the callers that already speak JSON.
	var req domain.UpdateBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" {
		req.LegalEntityID = r.URL.Query().Get("legal_entity_id")
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.URL.Query().Get("correlation_id")
	}
	if req.CorrelationID == "" {
		req.CorrelationID = uuid.NewString()
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRoleManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	current, err := h.store.GetBundle(r.Context(), roleDefinitionID, bundleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if !current.ActiveFlag {
		writeJSON(w, http.StatusOK, current)
		return
	}

	scope := clients.Scope{
		PrincipalID:   principalID,
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		CorrelationID: req.CorrelationID,
	}
	if err := h.authzAdmin.SetPermissionBundleActive(r.Context(), roleDefinitionID, current.BundleCode, false, scope); err != nil {
		h.log.Error("failed to retire permission bundle in authorization-svc",
			zap.String("bundle_id", bundleID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
		return
	}

	detached := false
	updated, err := h.store.UpdateBundle(r.Context(), roleDefinitionID, bundleID, nil, &detached, principalID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	h.publisher.PublishBundleUpdated(r.Context(), *updated, principalID)
	writeJSON(w, http.StatusOK, updated)
}

// ── GET /v1/permission-bundles ────────────────────────────────────────────────

// ListAllBundles reads the tenant's whole bundle catalogue — every role's
// bundles in one read — optionally narrowed by role_id, active_flag, a code
// search, and paging. This is the view the detach flow and cross-role reviews
// need, and the role-scoped ListBundles cannot provide.
func (h *Handler) ListAllBundles(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.BundleListFilter{
		RoleID: r.URL.Query().Get("role_id"),
		Query:  r.URL.Query().Get("search"),
	}
	if raw := r.URL.Query().Get("active_flag"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_active_flag", "active_flag must be true or false")
			return
		}
		filter.ActiveFlag = &v
	}
	limit, err := parseIntQuery(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	offset, err := parseIntQuery(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_offset", err.Error())
		return
	}
	filter.Limit, filter.Offset = limit, offset

	list, err := h.store.ListAllBundles(r.Context(), filter)
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

// parseIntQuery reads a non-negative integer query parameter, defaulting to 0
// when absent.
func parseIntQuery(r *http.Request, key string) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", key, raw)
	}
	return n, nil
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

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", string(domain.ErrTenantMissing))
		return "", false
	}
	return tenantID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrRoleNotFound) || errors.Is(err, domain.ErrBundleNotFound) {
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
