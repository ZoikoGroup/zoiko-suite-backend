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
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/telemetry"
)

type Store interface {
	// CreateRole records the definition AND enqueues role.created in the same
	// transaction. actorID is the verified caller the event is attributed to.
	CreateRole(ctx context.Context, r *domain.RoleDefinition, actorID string) (created bool, err error)
	// RoleCodeTaken is asked BEFORE provisioning into authorization-svc, so a
	// duplicate is refused without leaving an orphan role there.
	//
	// exceptCorrelationID is what keeps a REPLAY from reading as a conflict. A
	// retry of the same create carries the same correlation id and the same
	// role code; without the exclusion the second attempt is refused 409 and
	// idempotency is gone, which is the opposite of what a retried write needs.
	RoleCodeTaken(ctx context.Context, roleCode, exceptCorrelationID string) (bool, error)
	GetRole(ctx context.Context, roleDefinitionID string) (*domain.RoleDefinition, error)
	ListRoles(ctx context.Context, filter domain.ListFilter) ([]domain.RoleDefinition, error)
	UpdateRole(ctx context.Context, roleDefinitionID, roleName, status, updatedByPrincipalID string) (*domain.RoleDefinition, error)

	CreateBundle(ctx context.Context, b *domain.PermissionBundleDef, actorID string) (created bool, err error)
	BundleCodeTaken(ctx context.Context, roleDefinitionID, bundleCode, exceptCorrelationID string) (bool, error)
	ListBundles(ctx context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error)
	GetBundle(ctx context.Context, roleDefinitionID, bundleID string) (*domain.PermissionBundleDef, error)
	UpdateBundle(ctx context.Context, roleDefinitionID, bundleID string, permittedActions []string, activeFlag *bool, updatedByPrincipalID string) (*domain.PermissionBundleDef, error)
	ListAllBundles(ctx context.Context, filter domain.BundleListFilter) ([]domain.PermissionBundleDef, error)
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

// actionRoleManage is the action every write on this service is authorized
// against, on the legal entity in the request body.
//
// ── IT IS "ROLE_MANAGE", NOT "ACCESS_ROLE_MANAGE" ───────────────────────────
//
// It used to be the latter, and that name exists nowhere else in this estate.
// deployments/scripts/seed-demo-rbac.ps1 attaches ACCESS_CONTROL_FULL with
// permitted_actions ["ROLE_MANAGE"] and says in its own comment that "the
// service authorizes ROLE_MANAGE against the legal_entity_id in the request
// body". The console tells the operator a 403 means they "hold no ROLE_MANAGE
// grant". The live bundle in authorization-svc grants ROLE_MANAGE. The service
// was the only party asking for the other name.
//
// So every write this service served was refused 403 "authorization denied for
// this access control action", while every read worked — which presents as an
// operator who has not been granted enough, not as a service asking for an
// action nobody defines. authorization-svc's decision log records it plainly:
// ROLE_MANAGE GRANTED via CONSOLE_DEMO_OPERATOR, then ACCESS_ROLE_MANAGE DENIED
// no_grant for the same principal on the same entity minutes later.
//
// There is deliberately no ACCESS_ROLE_VIEW counterpart. A dead constant of
// that name sat here unused, implying a read check that was never made. Reads
// stay authenticated (a principal OR a workload) and tenant-scoped by row-level
// security, and are not gated on a grant, for two reasons: no bundle anywhere
// in the estate grants a view action on this service, so enforcing one would
// close a working page against every existing operator; and
// identity-context-svc reads this catalogue as a WORKLOAD on its session
// resolution hot path, where a grant check has no principal to evaluate.
const actionRoleManage = telemetry.ActionRoleManage

type Handler struct {
	store      Store
	authz      AuthZClient
	authzAdmin AuthzAdmin
	metrics    *telemetry.Domain
	log        *zap.Logger
}

func New(store Store, authz AuthZClient, authzAdmin AuthzAdmin, metrics *telemetry.Domain, log *zap.Logger) *Handler {
	return &Handler{store: store, authz: authz, authzAdmin: authzAdmin, metrics: metrics, log: log}
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
// Idempotent on (tenant_id, correlation_id): a replay answers 200 with the
// original definition, a first write answers 201. The two used to be
// indistinguishable — every outcome was 201 — so a client could not tell a
// created role from a returned one, which is the entire value of an
// idempotency key.
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.roleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.RoleCode == "" || req.RoleName == "" || req.RoleScopeType == "" || req.CorrelationID == "" {
		h.roleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, role_code, role_name, role_scope_type, correlation_id are required")
		return
	}
	if req.RoleScopeType != "LEGAL_ENTITY" && req.RoleScopeType != "TENANT" {
		h.roleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "invalid_scope_type", "role_scope_type must be LEGAL_ENTITY or TENANT")
		return
	}

	principalID, ok := h.requirePrincipal(w, r, h.metrics.RoleWrites)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, h.metrics.RoleWrites)
	if !ok {
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID); err != nil {
		h.writeAuthzErr(w, err, h.metrics.RoleWrites)
		return
	}

	// Asked BEFORE provisioning. The UNIQUE (tenant_id, role_code) index is the
	// backstop and holds under a race, but reaching it only after the role had
	// been created in authorization-svc left a provisioned role there that this
	// register refused to record — a grant nobody here could see, retire or
	// explain. The earlier check costs one indexed lookup on a rare path.
	exists, err := h.store.RoleCodeTaken(r.Context(), req.RoleCode, req.CorrelationID)
	if err != nil {
		h.log.Error("failed to check role_code uniqueness", zap.Error(err))
		h.roleWrite(telemetry.WriteStoreUnavailable)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if exists {
		h.roleWrite(telemetry.WriteConflict)
		writeError(w, http.StatusConflict, "role_code_exists", string(domain.ErrRoleCodeExists))
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
		h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateRole, telemetry.AdminUnavailable).Inc()
		h.log.Error("failed to provision role in authorization-svc", zap.Error(err))
		h.roleWrite(telemetry.WriteAuthzAdminUnavail)
		writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
		return
	}
	h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateRole, telemetry.AdminOK).Inc()

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

	created, err := h.store.CreateRole(r.Context(), role, principalID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.RoleWrites)
		return
	}
	if created {
		h.roleWrite(telemetry.WriteCreated)
		writeJSON(w, http.StatusCreated, role)
		return
	}
	// A replay of a correlation id this tenant has already used. 200, not 201:
	// nothing was created by this call.
	h.roleWrite(telemetry.WriteReplayed)
	writeJSON(w, http.StatusOK, role)
}

// ── GET /v1/role-definitions ──────────────────────────────────────────────────

// ListRoles reads the role catalogue, optionally narrowed by status,
// scope_type, a code/name search, and limit/offset paging. Zero limit means
// no cap: paging is something a caller asks for, because a silently truncated
// catalogue would make its totals wrong.
func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r, nil)
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

	_, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r, nil)
	if !ok {
		return
	}

	role, err := h.store.GetRole(r.Context(), roleDefinitionID)
	if err != nil {
		h.writeStoreErr(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// ── PATCH /v1/role-definitions/{role_definition_id} ───────────────────────────

func (h *Handler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	var req domain.UpdateRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.roleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r, h.metrics.RoleWrites)
	if !ok {
		return
	}
	updateTenantID, ok := h.requireTenant(w, r, h.metrics.RoleWrites)
	if !ok {
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID); err != nil {
		h.writeAuthzErr(w, err, h.metrics.RoleWrites)
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
		h.roleWrite(telemetry.WriteInvalidRequest)
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
			h.writeStoreErr(w, err, h.metrics.RoleWrites)
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
				h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminSetRoleState, telemetry.AdminUnavailable).Inc()
				h.log.Error("failed to propagate role status to authorization-svc",
					zap.String("role_definition_id", roleDefinitionID),
					zap.String("status", req.Status),
					zap.Error(err))
				h.roleWrite(telemetry.WriteAuthzAdminUnavail)
				writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
				return
			}
			h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminSetRoleState, telemetry.AdminOK).Inc()
		}
	}

	updated, err := h.store.UpdateRole(r.Context(), roleDefinitionID, req.RoleName, req.Status, principalID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.RoleWrites)
		return
	}

	h.roleWrite(telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/role-definitions/{role_definition_id}/permission-bundles ────────

// CreateBundle records the bundle here AND provisions it into
// authorization-svc via the admin API, attached to the role provisioned at
// role-creation time.
//
// Idempotent on (tenant_id, correlation_id): 201 on a first write, 200 on a
// replay.
func (h *Handler) CreateBundle(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	var req domain.CreateBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.BundleCode == "" || len(req.PermittedActions) == 0 || req.CorrelationID == "" {
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "missing_fields", "bundle_code, permitted_actions, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r, h.metrics.BundleWrites)
	if !ok {
		return
	}
	bundleTenantID, ok := h.requireTenant(w, r, h.metrics.BundleWrites)
	if !ok {
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID); err != nil {
		h.writeAuthzErr(w, err, h.metrics.BundleWrites)
		return
	}

	if _, err := h.store.GetRole(r.Context(), roleDefinitionID); err != nil {
		h.writeStoreErr(w, err, h.metrics.BundleWrites)
		return
	}

	// Asked before provisioning, and this one is not merely tidy. Attaching in
	// authorization-svc is an upsert-REPLACE on (role_id, bundle_code), so a
	// create this register is about to refuse would still have overwritten the
	// permitted actions of the bundle that already holds the code.
	exists, err := h.store.BundleCodeTaken(r.Context(), roleDefinitionID, req.BundleCode, req.CorrelationID)
	if err != nil {
		h.log.Error("failed to check bundle_code uniqueness", zap.Error(err))
		h.bundleWrite(telemetry.WriteStoreUnavailable)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if exists {
		h.bundleWrite(telemetry.WriteConflict)
		writeError(w, http.StatusConflict, "bundle_code_exists", string(domain.ErrBundleCodeExists))
		return
	}

	if err := h.authzAdmin.CreatePermissionBundle(r.Context(), roleDefinitionID, req.BundleCode, req.PermittedActions, clients.Scope{
		PrincipalID:   principalID,
		TenantID:      bundleTenantID,
		LegalEntityID: req.LegalEntityID,
		CorrelationID: req.CorrelationID,
	}); err != nil {
		h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, telemetry.AdminUnavailable).Inc()
		h.log.Error("failed to provision permission bundle in authorization-svc", zap.Error(err))
		h.bundleWrite(telemetry.WriteAuthzAdminUnavail)
		writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
		return
	}
	h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, telemetry.AdminOK).Inc()

	now := time.Now().UTC()
	bundle := &domain.PermissionBundleDef{
		BundleID:         uuid.NewString(),
		TenantID:         bundleTenantID,
		RoleDefinitionID: roleDefinitionID,
		BundleCode:       req.BundleCode,
		PermittedActions: req.PermittedActions,
		ActiveFlag:       true,
		CorrelationID:    req.CorrelationID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	created, err := h.store.CreateBundle(r.Context(), bundle, principalID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.BundleWrites)
		return
	}
	if created {
		h.bundleWrite(telemetry.WriteCreated)
		writeJSON(w, http.StatusCreated, bundle)
		return
	}
	h.bundleWrite(telemetry.WriteReplayed)
	writeJSON(w, http.StatusOK, bundle)
}

// ── GET /v1/role-definitions/{role_definition_id}/permission-bundles ─────────

func (h *Handler) ListBundles(w http.ResponseWriter, r *http.Request) {
	roleDefinitionID := chi.URLParam(r, "role_definition_id")

	_, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r, nil)
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

	_, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r, nil)
	if !ok {
		return
	}

	bundle, err := h.store.GetBundle(r.Context(), roleDefinitionID, bundleID)
	if err != nil {
		h.writeStoreErr(w, err, nil)
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
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = uuid.NewString()
	}

	principalID, ok := h.requirePrincipal(w, r, h.metrics.BundleWrites)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, h.metrics.BundleWrites)
	if !ok {
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID); err != nil {
		h.writeAuthzErr(w, err, h.metrics.BundleWrites)
		return
	}

	// A partial update that changes nothing would be a no-op write; refuse it
	// rather than silently accept a request whose intent cannot be read. Both
	// fields absent is far more likely a malformed client than a deliberate
	// replay — a genuine replay sends the same values and is handled below.
	if req.PermittedActions == nil && req.ActiveFlag == nil {
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "nothing_to_update", "supply permitted_actions and/or active_flag")
		return
	}
	if req.PermittedActions != nil && len(req.PermittedActions) == 0 {
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "empty_actions", "a bundle must permit at least one action; set active_flag to withdraw the bundle instead")
		return
	}

	current, err := h.store.GetBundle(r.Context(), roleDefinitionID, bundleID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.BundleWrites)
		return
	}

	actionsChanged := req.PermittedActions != nil && !slices.Equal(current.PermittedActions, req.PermittedActions)
	activeChanged := req.ActiveFlag != nil && *req.ActiveFlag != current.ActiveFlag
	if !actionsChanged && !activeChanged {
		h.bundleWrite(telemetry.WriteNoChange)
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
			h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, telemetry.AdminUnavailable).Inc()
			h.log.Error("failed to propagate permission bundle actions to authorization-svc",
				zap.String("bundle_id", bundleID), zap.Error(err))
			h.bundleWrite(telemetry.WriteAuthzAdminUnavail)
			writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
			return
		}
		h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, telemetry.AdminOK).Inc()
	}
	if activeChanged {
		if err := h.authzAdmin.SetPermissionBundleActive(r.Context(), roleDefinitionID, current.BundleCode, *req.ActiveFlag, scope); err != nil {
			h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminSetBundle, telemetry.AdminUnavailable).Inc()
			h.log.Error("failed to propagate permission bundle active state to authorization-svc",
				zap.String("bundle_id", bundleID), zap.Error(err))
			h.bundleWrite(telemetry.WriteAuthzAdminUnavail)
			writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
			return
		}
		h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminSetBundle, telemetry.AdminOK).Inc()
	}

	updated, err := h.store.UpdateBundle(r.Context(), roleDefinitionID, bundleID, req.PermittedActions, req.ActiveFlag, principalID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.BundleWrites)
		return
	}

	h.bundleWrite(telemetry.WriteUpdated)
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

	principalID, ok := h.requirePrincipal(w, r, h.metrics.BundleWrites)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r, h.metrics.BundleWrites)
	if !ok {
		return
	}

	// DELETE carries no body over this API surface; the request context rides
	// the query string instead (legal_entity_id, correlation_id). A body is
	// still honoured if present, for the callers that already speak JSON.
	var req domain.UpdateBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" {
		req.LegalEntityID = r.URL.Query().Get("legal_entity_id")
	}
	if req.LegalEntityID == "" {
		h.bundleWrite(telemetry.WriteInvalidRequest)
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.URL.Query().Get("correlation_id")
	}
	if req.CorrelationID == "" {
		req.CorrelationID = uuid.NewString()
	}

	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID); err != nil {
		h.writeAuthzErr(w, err, h.metrics.BundleWrites)
		return
	}

	current, err := h.store.GetBundle(r.Context(), roleDefinitionID, bundleID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.BundleWrites)
		return
	}
	if !current.ActiveFlag {
		h.bundleWrite(telemetry.WriteAlreadyDetachedNoop)
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
		h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminSetBundle, telemetry.AdminUnavailable).Inc()
		h.log.Error("failed to retire permission bundle in authorization-svc",
			zap.String("bundle_id", bundleID), zap.Error(err))
		h.bundleWrite(telemetry.WriteAuthzAdminUnavail)
		writeError(w, http.StatusServiceUnavailable, "authz_admin_unavailable", err.Error())
		return
	}
	h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminSetBundle, telemetry.AdminOK).Inc()

	detached := false
	updated, err := h.store.UpdateBundle(r.Context(), roleDefinitionID, bundleID, nil, &detached, principalID)
	if err != nil {
		h.writeStoreErr(w, err, h.metrics.BundleWrites)
		return
	}

	h.bundleWrite(telemetry.WriteDetached)
	writeJSON(w, http.StatusOK, updated)
}

// ── GET /v1/permission-bundles ────────────────────────────────────────────────

// ListAllBundles reads the tenant's whole bundle catalogue — every role's
// bundles in one read — optionally narrowed by role_id, active_flag, a code
// search, and paging. This is the view the detach flow and cross-role reviews
// need, and the role-scoped ListBundles cannot provide.
func (h *Handler) ListAllBundles(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r, nil)
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

// checkAllowed is AuthZClient.CheckAllowed plus the counter, so no call site
// can add an authorization check and forget to make it visible.
func (h *Handler) checkAllowed(ctx context.Context, principalID, legalEntityID string) error {
	err := h.authz.CheckAllowed(ctx, principalID, legalEntityID, actionRoleManage)
	switch {
	case err == nil:
		h.metrics.AuthZDecisions.WithLabelValues(actionRoleManage, telemetry.AuthZGranted).Inc()
	case errors.Is(err, domain.ErrAuthorizationDenied):
		h.metrics.AuthZDecisions.WithLabelValues(actionRoleManage, telemetry.AuthZDenied).Inc()
	default:
		h.metrics.AuthZDecisions.WithLabelValues(actionRoleManage, telemetry.AuthZUnavailable).Inc()
	}
	return err
}

// count increments an outcome on a counter that may be nil.
//
// nil is the read paths: they have no write-outcome series, and passing one
// would file a read refusal under a counter named for writes. A nil-tolerant
// helper keeps the refusal helpers shared between reads and writes instead of
// duplicating them per path, which is how the two drifted apart before.
func count(c *prometheus.CounterVec, outcome string) {
	if c == nil {
		return
	}
	c.WithLabelValues(outcome).Inc()
}

func (h *Handler) roleWrite(outcome string)   { h.metrics.RoleWrites.WithLabelValues(outcome).Inc() }
func (h *Handler) bundleWrite(outcome string) { h.metrics.BundleWrites.WithLabelValues(outcome).Inc() }

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request, counter *prometheus.CounterVec) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		count(counter, telemetry.WriteIdentityMissing)
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

// requireCaller accepts EITHER a human principal or a workload identity.
//
// The canonical input contract defines actor_subject_id as "X-Principal-Id for
// a human subject or X-Workload-Id for a workload" — both satisfy it. Reads
// that only need to know the caller is somebody, and never attribute anything
// to them, must honour both halves; requirePrincipal honours only the first.
//
// This was not a theoretical gap. identity-context-svc reads
// GET /v1/role-definitions/{id}/permission-bundles on its session-resolution
// hot path and identifies itself, correctly, as X-Workload-Id. It was refused
// 401 identity_missing, which its resolver reported fail-closed as "upstream
// dependency unavailable" — so every context resolution in the stack returned
// 503, and the reason looked like an outage rather than a contract mismatch.
//
// Writes keep requirePrincipal. A workload may read the catalogue; only a
// named human changes it, because the principal is what the evidence records.
func (h *Handler) requireCaller(w http.ResponseWriter, r *http.Request) (string, bool) {
	if principalID := r.Header.Get("X-Principal-Id"); principalID != "" {
		return principalID, true
	}
	if workloadID := r.Header.Get("X-Workload-Id"); workloadID != "" {
		return workloadID, true
	}
	writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
	return "", false
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request, counter *prometheus.CounterVec) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		count(counter, telemetry.WriteIdentityMissing)
		writeError(w, http.StatusUnauthorized, "tenant_missing", string(domain.ErrTenantMissing))
		return "", false
	}
	return tenantID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error, counter *prometheus.CounterVec) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		count(counter, telemetry.WriteForbidden)
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	count(counter, telemetry.WriteAuthzUnavailable)
	writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
}

// writeStoreErr renders a store failure.
//
// The conflict cases come first and are answered 409 with their own error
// codes. Both used to arrive here as a bare UNIQUE violation and leave as 503
// store_unavailable with the raw SQLSTATE text in the body — a duplicate role
// code reported as a database outage, pointing on-call at a database that was
// working perfectly.
func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, counter *prometheus.CounterVec) {
	switch {
	case errors.Is(err, domain.ErrRoleCodeExists):
		count(counter, telemetry.WriteConflict)
		writeError(w, http.StatusConflict, "role_code_exists", err.Error())
	case errors.Is(err, domain.ErrBundleCodeExists):
		count(counter, telemetry.WriteConflict)
		writeError(w, http.StatusConflict, "bundle_code_exists", err.Error())
	case errors.Is(err, domain.ErrRoleNotFound), errors.Is(err, domain.ErrBundleNotFound):
		count(counter, telemetry.WriteNotFound)
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		h.log.Error("access control store error", zap.Error(err))
		count(counter, telemetry.WriteStoreUnavailable)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
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
