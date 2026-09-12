package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/handler"
	"zoiko.io/access-control-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	rolesByID            map[string]*domain.RoleDefinition
	rolesByCorrelation   map[string]*domain.RoleDefinition
	bundlesByRole        map[string][]domain.PermissionBundleDef
	bundlesByCorrelation map[string]*domain.PermissionBundleDef
	bundlesByID          map[string]*domain.PermissionBundleDef
}

func newStubStore() *stubStore {
	return &stubStore{
		rolesByID:            make(map[string]*domain.RoleDefinition),
		rolesByCorrelation:   make(map[string]*domain.RoleDefinition),
		bundlesByRole:        make(map[string][]domain.PermissionBundleDef),
		bundlesByCorrelation: make(map[string]*domain.PermissionBundleDef),
		bundlesByID:          make(map[string]*domain.PermissionBundleDef),
	}
}

func (s *stubStore) CreateRole(_ context.Context, r *domain.RoleDefinition) (bool, error) {
	if existing, ok := s.rolesByCorrelation[r.CorrelationID]; ok {
		*r = *existing
		return false, nil
	}
	cp := *r
	s.rolesByID[r.RoleDefinitionID] = &cp
	s.rolesByCorrelation[r.CorrelationID] = &cp
	return true, nil
}

func (s *stubStore) GetRole(_ context.Context, roleDefinitionID string) (*domain.RoleDefinition, error) {
	r, ok := s.rolesByID[roleDefinitionID]
	if !ok {
		return nil, domain.ErrRoleNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) ListRoles(_ context.Context, filter domain.ListFilter) ([]domain.RoleDefinition, error) {
	var out []domain.RoleDefinition
	for _, r := range s.rolesByID {
		if filter.Status != "" && string(r.Status) != filter.Status {
			continue
		}
		if filter.ScopeType != "" && r.RoleScopeType != filter.ScopeType {
			continue
		}
		if filter.Query != "" && !strings.Contains(strings.ToLower(r.RoleCode), strings.ToLower(filter.Query)) && !strings.Contains(strings.ToLower(r.RoleName), strings.ToLower(filter.Query)) {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) UpdateRole(_ context.Context, roleDefinitionID, roleName, status, updatedByPrincipalID string) (*domain.RoleDefinition, error) {
	r, ok := s.rolesByID[roleDefinitionID]
	if !ok {
		return nil, domain.ErrRoleNotFound
	}
	if roleName != "" {
		r.RoleName = roleName
	}
	if status != "" {
		r.Status = domain.RoleStatus(status)
	}
	r.UpdatedByPrincipalID = updatedByPrincipalID
	r.UpdatedAt = time.Now().UTC()
	cp := *r
	return &cp, nil
}

func (s *stubStore) CreateBundle(_ context.Context, b *domain.PermissionBundleDef) (bool, error) {
	if existing, ok := s.bundlesByCorrelation[b.CorrelationID]; ok {
		*b = *existing
		return false, nil
	}
	cp := *b
	s.bundlesByRole[b.RoleDefinitionID] = append(s.bundlesByRole[b.RoleDefinitionID], cp)
	s.bundlesByCorrelation[b.CorrelationID] = &cp
	s.bundlesByID[b.BundleID] = &cp
	return true, nil
}

func (s *stubStore) ListBundles(_ context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error) {
	return s.bundlesByRole[roleDefinitionID], nil
}

func (s *stubStore) GetBundle(_ context.Context, roleDefinitionID, bundleID string) (*domain.PermissionBundleDef, error) {
	b, ok := s.bundlesByID[bundleID]
	if !ok || b.RoleDefinitionID != roleDefinitionID {
		return nil, domain.ErrBundleNotFound
	}
	cp := *b
	return &cp, nil
}

func (s *stubStore) UpdateBundle(_ context.Context, roleDefinitionID, bundleID string, permittedActions []string, activeFlag *bool, updatedByPrincipalID string) (*domain.PermissionBundleDef, error) {
	b, ok := s.bundlesByID[bundleID]
	if !ok || b.RoleDefinitionID != roleDefinitionID {
		return nil, domain.ErrBundleNotFound
	}
	if permittedActions != nil {
		b.PermittedActions = permittedActions
	}
	if activeFlag != nil {
		b.ActiveFlag = *activeFlag
	}
	b.UpdatedByPrincipalID = updatedByPrincipalID
	b.UpdatedAt = time.Now().UTC()
	cp := *b
	return &cp, nil
}

func (s *stubStore) ListAllBundles(_ context.Context, filter domain.BundleListFilter) ([]domain.PermissionBundleDef, error) {
	var out []domain.PermissionBundleDef
	for _, b := range s.bundlesByID {
		if filter.RoleID != "" && b.RoleDefinitionID != filter.RoleID {
			continue
		}
		if filter.ActiveFlag != nil && b.ActiveFlag != *filter.ActiveFlag {
			continue
		}
		out = append(out, *b)
	}
	return out, nil
}

type stubPublisher struct {
	roleCreated, roleUpdated, bundleUpdated int
}

func (p *stubPublisher) PublishRoleCreated(_ context.Context, _ domain.RoleDefinition, _ string) {
	p.roleCreated++
}
func (p *stubPublisher) PublishRoleUpdated(_ context.Context, _ domain.RoleDefinition, _ string) {
	p.roleUpdated++
}
func (p *stubPublisher) PublishBundleUpdated(_ context.Context, _ domain.PermissionBundleDef, _ string) {
	p.bundleUpdated++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type bundleActiveCall struct {
	roleID     string
	bundleCode string
	active     bool
}

type stubAuthzAdmin struct {
	createRoleErr   error
	createBundleErr error

	// setRoleActiveErr simulates authorization-svc being unreachable on the
	// propagation path, which must fail the PATCH rather than record a
	// retirement the platform is not enforcing.
	setRoleActiveErr  error
	setRoleActiveWant []bool

	// setBundleActiveErr and setBundleActiveCalls mirror the role-side
	// recorders for the bundle retire/reactivate propagation path.
	setBundleActiveErr   error
	setBundleActiveCalls []bundleActiveCall

	// The scope each call was made with. Recorded because two of these three
	// methods used to be invoked with an empty principal and tenant, which
	// authorization-svc's admin routes reject — and nothing here noticed,
	// because the parameters were positional strings the stub discarded.
	// Asserting on them is what stops that regressing.
	gotScopes []clients.Scope
}

func (a *stubAuthzAdmin) CreateRole(_ context.Context, _, _, _, _ string, s clients.Scope) error {
	a.gotScopes = append(a.gotScopes, s)
	return a.createRoleErr
}
func (a *stubAuthzAdmin) CreatePermissionBundle(_ context.Context, _, _ string, _ []string, s clients.Scope) error {
	a.gotScopes = append(a.gotScopes, s)
	return a.createBundleErr
}
func (a *stubAuthzAdmin) SetRoleActive(_ context.Context, _ string, active bool, s clients.Scope) error {
	a.setRoleActiveWant = append(a.setRoleActiveWant, active)
	a.gotScopes = append(a.gotScopes, s)
	return a.setRoleActiveErr
}
func (a *stubAuthzAdmin) SetPermissionBundleActive(_ context.Context, roleID, bundleCode string, active bool, s clients.Scope) error {
	a.setBundleActiveCalls = append(a.setBundleActiveCalls, bundleActiveCall{roleID: roleID, bundleCode: bundleCode, active: active})
	a.gotScopes = append(a.gotScopes, s)
	return a.setBundleActiveErr
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, admin *stubAuthzAdmin) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, admin, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func roleBody(correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id": "le-us",
		"role_code":       "PROCUREMENT_OFFICER",
		"role_name":       "Procurement Officer",
		"role_scope_type": "LEGAL_ENTITY",
		"correlation_id":  correlationID,
	}
}

// ── CreateRole tests ──────────────────────────────────────────────────────────

func TestCreateRole_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateRole_AuthzAdminUnavailable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{createRoleErr: domain.ErrAuthzAdminUnavailable})
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateRole_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubAuthzAdmin{})
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var role domain.RoleDefinition
	_ = json.NewDecoder(rr.Body).Decode(&role)
	if role.Status != domain.RoleStatusActive {
		t.Errorf("expected ACTIVE got %q", role.Status)
	}
	if pub.roleCreated != 1 {
		t.Errorf("expected 1 role.created event, got %d", pub.roleCreated)
	}
}

func TestCreateRole_IdempotentReplay(t *testing.T) {
	correlationID := uuid.NewString()
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})

	rr1 := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(correlationID), "admin-1")
	var role1 domain.RoleDefinition
	_ = json.NewDecoder(rr1.Body).Decode(&role1)

	rr2 := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(correlationID), "admin-1")
	var role2 domain.RoleDefinition
	_ = json.NewDecoder(rr2.Body).Decode(&role2)

	if role2.RoleDefinitionID != role1.RoleDefinitionID {
		t.Fatalf("retried create resolved to a different role_definition_id (%s) than the original (%s)", role2.RoleDefinitionID, role1.RoleDefinitionID)
	}
}

// ── CreateBundle tests ─────────────────────────────────────────────────────────

func createRole(t *testing.T, r chi.Router) domain.RoleDefinition {
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("role setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var role domain.RoleDefinition
	_ = json.NewDecoder(rr.Body).Decode(&role)
	return role
}

func bundleBody(correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id":   "le-us",
		"bundle_code":       "PO_FULL",
		"permitted_actions": []string{"PO_ISSUE", "PO_AMEND", "PO_CLOSE"},
		"correlation_id":    correlationID,
	}
}

func TestCreateBundle_RoleNotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/nonexistent-role/permission-bundles", bundleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateBundle_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubAuthzAdmin{})
	role := createRole(t, r)

	rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var bundle domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&bundle)
	if len(bundle.PermittedActions) != 3 {
		t.Errorf("expected 3 permitted actions, got %d", len(bundle.PermittedActions))
	}
	if pub.bundleUpdated != 1 {
		t.Errorf("expected 1 permission.bundle.updated event, got %d", pub.bundleUpdated)
	}
}

func TestCreateBundle_AuthzAdminUnavailable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{createBundleErr: domain.ErrAuthzAdminUnavailable})
	role := createRole(t, r)

	rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── UpdateRole tests ───────────────────────────────────────────────────────────

func TestUpdateRole_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubAuthzAdmin{})
	role := createRole(t, r)

	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID, map[string]any{"legal_entity_id": "le-us", "status": "RETIRED"}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.RoleDefinition
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if updated.Status != domain.RoleStatusRetired {
		t.Errorf("expected RETIRED got %q", updated.Status)
	}
	if pub.roleUpdated != 1 {
		t.Errorf("expected 1 role.updated event, got %d", pub.roleUpdated)
	}
}

// TestUpdateRole_RetirementReachesAuthorizationSvc is the test that matters
// most in this file.
//
// Status here is a label on a row; active_flag in authorization-svc is what
// FindGrantedActions joins through, and therefore the only thing that stops a
// role granting anything. Before this, PATCH status=RETIRED wrote the label and
// made no remote call, so every principal holding the role kept every action it
// granted while this register displayed RETIRED. Asserting the 200 alone would
// not have caught that -- the 200 was always there.
func TestUpdateRole_RetirementReachesAuthorizationSvc(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	role := createRole(t, r)

	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "status": "RETIRED"}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setRoleActiveWant) != 1 {
		t.Fatalf("retiring a role made %d SetRoleActive calls, expected 1 -- without it the role stays enforceable", len(admin.setRoleActiveWant))
	}
	if admin.setRoleActiveWant[0] != false {
		t.Fatalf("retirement asked authorization-svc for active=%v, expected false", admin.setRoleActiveWant[0])
	}
}

func TestUpdateRole_ReactivationAsksForActiveTrue(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	role := createRole(t, r)

	// ACTIVE -> RETIRED -> ACTIVE. The second transition must ask for true.
	_ = doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "status": "RETIRED"}, "admin-1")
	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "status": "ACTIVE"}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setRoleActiveWant) != 2 {
		t.Fatalf("expected 2 propagation calls across two transitions, got %d", len(admin.setRoleActiveWant))
	}
	if admin.setRoleActiveWant[1] != true {
		t.Fatalf("reactivation asked for active=%v, expected true", admin.setRoleActiveWant[1])
	}
}

// TestUpdateRole_AuthzAdminDown_RefusesTheStatusChange is the fail-closed case.
// An unreachable authorization-svc must NOT leave the catalogue claiming a
// retirement the platform is still not enforcing.
func TestUpdateRole_AuthzAdminDown_RefusesTheStatusChange(t *testing.T) {
	admin := &stubAuthzAdmin{}
	store := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(store, pub, &stubAuthZ{}, admin)
	role := createRole(t, r)

	admin.setRoleActiveErr = errors.New("authorization-svc admin API unreachable")

	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "status": "RETIRED"}, "admin-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the status change could not be propagated, got %d: %s", rr.Code, rr.Body.String())
	}
	// The label must not have moved, and nothing may have been announced.
	if got := store.rolesByID[role.RoleDefinitionID].Status; got != domain.RoleStatusActive {
		t.Errorf("status is %q after a refused retirement; the catalogue now disagrees with what is enforced", got)
	}
	if pub.roleUpdated != 0 {
		t.Errorf("published %d role.updated events for a retirement that did not happen", pub.roleUpdated)
	}
}

// TestUpdateRole_RenameOnlyDoesNotTouchAuthorizationSvc -- a rename changes no
// enforcement, so it must not make a remote call that could fail and block it.
func TestUpdateRole_RenameOnlyDoesNotTouchAuthorizationSvc(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	role := createRole(t, r)

	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "role_name": "Renamed Officer"}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setRoleActiveWant) != 0 {
		t.Fatalf("a rename made %d SetRoleActive calls, expected none", len(admin.setRoleActiveWant))
	}
}

// TestUpdateRole_NoOpStatusIsNotPropagated -- PATCHing the status a role
// already has changes nothing to enforce.
func TestUpdateRole_NoOpStatusIsNotPropagated(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	role := createRole(t, r) // created ACTIVE

	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "status": "ACTIVE"}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setRoleActiveWant) != 0 {
		t.Fatalf("a no-op status change made %d SetRoleActive calls, expected none", len(admin.setRoleActiveWant))
	}
}

// TestUpdateRole_UnknownStatusRejected -- status was a bare VARCHAR(20) with the
// vocabulary only in a comment, so any string persisted and then read back as
// neither ACTIVE nor RETIRED.
func TestUpdateRole_UnknownStatusRejected(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	role := createRole(t, r)

	rr := doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID,
		map[string]any{"legal_entity_id": "le-us", "status": "BANANA"}, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown status, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setRoleActiveWant) != 0 {
		t.Fatalf("an invalid status still reached authorization-svc (%d calls)", len(admin.setRoleActiveWant))
	}
}

// ── GetBundle tests ───────────────────────────────────────────────────────────

func createBundle(t *testing.T, r chi.Router) domain.PermissionBundleDef {
	role := createRole(t, r)
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("bundle setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var bundle domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&bundle)
	return bundle
}

func TestGetBundle_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	bundle := createBundle(t, r)

	rr := doReq(r, http.MethodGet, "/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID, nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.BundleID != bundle.BundleID {
		t.Fatalf("expected bundle %s got %s", bundle.BundleID, got.BundleID)
	}
}

func TestGetBundle_NotOwnedByRole(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})

	// Two roles so a bundle can be addressed under a role that does not own it.
	var roleID1, roleID2 string
	for i := 0; i < 2; i++ {
		rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
		if rr.Code != http.StatusCreated {
			t.Fatalf("role setup failed: %d", rr.Code)
		}
	}
	for id := range store.rolesByID {
		if roleID1 == "" {
			roleID1 = id
		} else if roleID2 == "" {
			roleID2 = id
		}
	}

	bundle := createBundle(t, r) // attached to roleID2 (last created)
	rr := doReq(r, http.MethodGet, "/v1/role-definitions/"+roleID1+"/permission-bundles/"+bundle.BundleID, nil, "admin-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a bundle addressed under the wrong role, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── UpdateBundle tests ────────────────────────────────────────────────────────

func TestUpdateBundle_EditActionsHappyPath(t *testing.T) {
	pub := &stubPublisher{}
	admin := &stubAuthzAdmin{}
	store := newStubStore()
	r := newRouter(store, pub, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil // drop the create-time scopes
	pub.bundleUpdated = 0

	rr := doReq(r, http.MethodPatch,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID,
		map[string]any{"legal_entity_id": "le-us", "permitted_actions": []string{"PO_ISSUE", "PO_CLOSE"}}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if len(updated.PermittedActions) != 2 {
		t.Errorf("expected 2 actions, got %d", len(updated.PermittedActions))
	}
	if updated.UpdatedByPrincipalID != "admin-1" {
		t.Errorf("updated_by_principal_id not stamped: got %q", updated.UpdatedByPrincipalID)
	}
	if got := store.bundlesByID[bundle.BundleID].PermittedActions; len(got) != 2 {
		t.Errorf("stub store not updated: %v", got)
	}
	if pub.bundleUpdated != 1 {
		t.Errorf("expected 1 permission.bundle.updated event, got %d", pub.bundleUpdated)
	}
	// The edit propagates as an upsert-replace on (role_id, bundle_code).
	if len(admin.gotScopes) != 1 {
		t.Fatalf("expected 1 admin propagation call, got %d", len(admin.gotScopes))
	}
	if admin.gotScopes[0].LegalEntityID != "le-us" {
		t.Errorf("scope legal entity not forwarded: got %q", admin.gotScopes[0].LegalEntityID)
	}
}

// TestUpdateBundle_ActiveFlagFalseClearsEnforcement -- turning a bundle off
// must reach authorization-svc's retire endpoint, because active_flag is in
// the JOIN of both evaluation reads there. Without the call the PATCH would
// record a withdrawal the platform still enforces.
func TestUpdateBundle_ActiveFlagFalseClearsEnforcement(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil

	rr := doReq(r, http.MethodPatch,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID,
		map[string]any{"legal_entity_id": "le-us", "active_flag": false}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setBundleActiveCalls) != 1 {
		t.Fatalf("expected 1 bundle retire propagation, got %d", len(admin.setBundleActiveCalls))
	}
	call := admin.setBundleActiveCalls[0]
	if call.active {
		t.Fatalf("asked authorization-svc for active=%v on a detach, expected false", call.active)
	}
	if call.bundleCode != bundle.BundleCode {
		t.Errorf("retire resolved the wrong code: got %q want %q", call.bundleCode, bundle.BundleCode)
	}
	var got domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.ActiveFlag {
		t.Error("bundle still active after a detach PATCH")
	}
}

func TestUpdateBundle_NoOpReplayReturnsCurrent(t *testing.T) {
	admin := &stubAuthzAdmin{}
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil
	pub.bundleUpdated = 0

	rr := doReq(r, http.MethodPatch,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID,
		map[string]any{"legal_entity_id": "le-us", "permitted_actions": bundle.PermittedActions}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.gotScopes) != 0 {
		t.Fatalf("a no-op edit made %d propagation calls, expected none", len(admin.gotScopes))
	}
	if pub.bundleUpdated != 0 {
		t.Errorf("published %d events for a no-op edit", pub.bundleUpdated)
	}
}

func TestUpdateBundle_EmptyActionsRejected(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil

	rr := doReq(r, http.MethodPatch,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID,
		map[string]any{"legal_entity_id": "le-us", "permitted_actions": []string{}}, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty action list, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.gotScopes) != 0 {
		t.Fatalf("an empty action list still reached authorization-svc (%d calls)", len(admin.gotScopes))
	}
}

func TestUpdateBundle_NothingToUpdateRejected(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil

	rr := doReq(r, http.MethodPatch,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID,
		map[string]any{"legal_entity_id": "le-us"}, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a body that changes nothing, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.gotScopes) != 0 {
		t.Fatalf("an empty edit still reached authorization-svc (%d calls)", len(admin.gotScopes))
	}
}

// TestUpdateBundle_AuthzAdminDown_RefusesTheEdit -- the fail-closed case, the
// same shape as the role-status one: an unreachable authorization-svc must not
// leave the register claiming an action list the platform is not enforcing.
func TestUpdateBundle_AuthzAdminDown_RefusesTheEdit(t *testing.T) {
	admin := &stubAuthzAdmin{}
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)

	admin.createBundleErr = errors.New("authorization-svc admin API unreachable")

	rr := doReq(r, http.MethodPatch,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID,
		map[string]any{"legal_entity_id": "le-us", "permitted_actions": []string{"PO_ISSUE"}}, "admin-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the edit could not be propagated, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := store.bundlesByID[bundle.BundleID].PermittedActions; len(got) != 3 {
		t.Errorf("actions changed to %v after a refused edit; the register now disagrees with what is enforced", got)
	}
}

// ── DetachBundle tests ────────────────────────────────────────────────────────

func TestDetachBundle_HappyPath(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil

	rr := doReq(r, http.MethodDelete,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID+"?legal_entity_id=le-us",
		nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setBundleActiveCalls) != 1 {
		t.Fatalf("expected 1 retire propagation, got %d", len(admin.setBundleActiveCalls))
	}
	if admin.setBundleActiveCalls[0].active {
		t.Fatal("detach asked authorization-svc for active=true")
	}
	var got domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.ActiveFlag {
		t.Error("bundle still active after detach")
	}
}

func TestDetachBundle_AlreadyDetachedIsIdempotent(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)
	admin.gotScopes = nil

	// Detach once, then again.
	_ = doReq(r, http.MethodDelete,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID+"?legal_entity_id=le-us",
		nil, "admin-1")
	admin.setBundleActiveCalls = nil

	rr := doReq(r, http.MethodDelete,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID+"?legal_entity_id=le-us",
		nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(admin.setBundleActiveCalls) != 0 {
		t.Fatalf("re-detaching made %d retire calls, expected none", len(admin.setBundleActiveCalls))
	}
}

func TestDetachBundle_RemoteDown_Refuses(t *testing.T) {
	admin := &stubAuthzAdmin{}
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)

	admin.setBundleActiveErr = errors.New("authorization-svc admin API unreachable")

	rr := doReq(r, http.MethodDelete,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID+"?legal_entity_id=le-us",
		nil, "admin-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the retire could not be propagated, got %d: %s", rr.Code, rr.Body.String())
	}
	if !store.bundlesByID[bundle.BundleID].ActiveFlag {
		t.Error("bundle was detached locally despite a refused retirement; the register now claims a state the platform is not enforcing")
	}
}

// ── ListAllBundles tests ──────────────────────────────────────────────────────

func TestListAllBundles_FlatCatalogue(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	createBundle(t, r)
	createBundle(t, r)

	rr := doReq(r, http.MethodGet, "/v1/permission-bundles/", nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var bundles []domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&bundles)
	if len(bundles) != 2 {
		t.Fatalf("expected 2 bundles across roles, got %d", len(bundles))
	}
}

func TestListAllBundles_FiltersByActiveFlag(t *testing.T) {
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin)
	bundle := createBundle(t, r)

	// Detach it, then the active=true read must be empty.
	_ = doReq(r, http.MethodDelete,
		"/v1/role-definitions/"+bundle.RoleDefinitionID+"/permission-bundles/"+bundle.BundleID+"?legal_entity_id=le-us",
		nil, "admin-1")

	rr := doReq(r, http.MethodGet, "/v1/permission-bundles?active_flag=true", nil, "admin-1")
	var active []domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&active)
	if len(active) != 0 {
		t.Fatalf("expected no active bundles after detach, got %d", len(active))
	}

	rr = doReq(r, http.MethodGet, "/v1/permission-bundles?active_flag=false", nil, "admin-1")
	var detached []domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&detached)
	if len(detached) != 1 {
		t.Fatalf("expected 1 detached bundle, got %d", len(detached))
	}
}

// ── ListRoles filter tests ────────────────────────────────────────────────────

func TestListRoles_InvalidStatusRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	rr := doReq(r, http.MethodGet, "/v1/role-definitions/?status=BANANA", nil, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown status filter, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListRoles_InvalidScopeTypeRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	rr := doReq(r, http.MethodGet, "/v1/role-definitions/?scope_type=PLANET", nil, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown scope_type filter, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListRoles_InvalidLimitRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})
	rr := doReq(r, http.MethodGet, "/v1/role-definitions/?limit=-1", nil, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a negative limit, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListRoles_SearchNarrowsResults(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{})

	req := roleBody(uuid.NewString())
	req["role_code"] = "AP_VENDOR_MANAGER"
	req["role_name"] = "Vendor Manager"
	if rr := doReq(r, http.MethodPost, "/v1/role-definitions/", req, "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("seed role: %d %s", rr.Code, rr.Body.String())
	}
	_ = createBundle(t, r) // this creates another role (PROCUREMENT_OFFICER)

	rr := doReq(r, http.MethodGet, "/v1/role-definitions/?search=AP_VENDOR", nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var roles []domain.RoleDefinition
	_ = json.NewDecoder(rr.Body).Decode(&roles)
	if len(roles) != 1 {
		t.Fatalf("expected 1 role matching the search, got %d", len(roles))
	}
	if roles[0].RoleCode != "AP_VENDOR_MANAGER" {
		t.Errorf("search returned %q, want AP_VENDOR_MANAGER", roles[0].RoleCode)
	}
}
