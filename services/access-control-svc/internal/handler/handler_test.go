package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

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
}

func newStubStore() *stubStore {
	return &stubStore{
		rolesByID:            make(map[string]*domain.RoleDefinition),
		rolesByCorrelation:   make(map[string]*domain.RoleDefinition),
		bundlesByRole:        make(map[string][]domain.PermissionBundleDef),
		bundlesByCorrelation: make(map[string]*domain.PermissionBundleDef),
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

func (s *stubStore) ListRoles(_ context.Context, status string) ([]domain.RoleDefinition, error) {
	var out []domain.RoleDefinition
	for _, r := range s.rolesByID {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) UpdateRole(_ context.Context, roleDefinitionID, roleName, status string) (*domain.RoleDefinition, error) {
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
	return true, nil
}

func (s *stubStore) ListBundles(_ context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error) {
	return s.bundlesByRole[roleDefinitionID], nil
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

type stubAuthzAdmin struct {
	createRoleErr   error
	createBundleErr error

	// setRoleActiveErr simulates authorization-svc being unreachable on the
	// propagation path, which must fail the PATCH rather than record a
	// retirement the platform is not enforcing.
	setRoleActiveErr  error
	setRoleActiveWant []bool
}

func (a *stubAuthzAdmin) CreateRole(_ context.Context, _, _, _, _, _, _, _ string) error {
	return a.createRoleErr
}
func (a *stubAuthzAdmin) CreatePermissionBundle(_ context.Context, _, _ string, _ []string, _ string) error {
	return a.createBundleErr
}
func (a *stubAuthzAdmin) SetRoleActive(_ context.Context, _ string, active bool, _ string) error {
	a.setRoleActiveWant = append(a.setRoleActiveWant, active)
	return a.setRoleActiveErr
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
