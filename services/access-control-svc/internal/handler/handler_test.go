package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/handler"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeStore struct {
	createRoleFunc           func(ctx context.Context, p domain.CreateRoleParams) (*domain.Role, bool, error)
	findRoleByIDFunc         func(ctx context.Context, id string) (*domain.Role, error)
	listRolesByTenantFunc    func(ctx context.Context, tenantID string) ([]domain.Role, error)
	deactivateRoleFunc       func(ctx context.Context, id string) (*domain.Role, error)
	createBundleFunc         func(ctx context.Context, p domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error)
	findBundleByIDFunc       func(ctx context.Context, id string) (*domain.PermissionBundle, error)
	listBundlesByTenantFunc  func(ctx context.Context, tenantID string) ([]domain.PermissionBundle, error)
	updateBundleActionsFunc  func(ctx context.Context, p domain.UpdatePermissionBundleActionsParams) (*domain.PermissionBundle, error)
	createLinkFunc           func(ctx context.Context, p domain.CreateRolePermissionBundleLinkParams) (*domain.RolePermissionBundleLink, error)
	removeLinkFunc           func(ctx context.Context, p domain.RemoveRolePermissionBundleLinkParams) error
	listBundlesForRoleFunc   func(ctx context.Context, roleID string) ([]domain.PermissionBundle, error)
}

func (f *fakeStore) CreateRole(ctx context.Context, p domain.CreateRoleParams) (*domain.Role, bool, error) {
	return f.createRoleFunc(ctx, p)
}
func (f *fakeStore) FindRoleByID(ctx context.Context, id string) (*domain.Role, error) {
	return f.findRoleByIDFunc(ctx, id)
}
func (f *fakeStore) ListRolesByTenant(ctx context.Context, tenantID string) ([]domain.Role, error) {
	return f.listRolesByTenantFunc(ctx, tenantID)
}
func (f *fakeStore) DeactivateRole(ctx context.Context, id string) (*domain.Role, error) {
	return f.deactivateRoleFunc(ctx, id)
}
func (f *fakeStore) CreatePermissionBundle(ctx context.Context, p domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error) {
	return f.createBundleFunc(ctx, p)
}
func (f *fakeStore) FindBundleByID(ctx context.Context, id string) (*domain.PermissionBundle, error) {
	return f.findBundleByIDFunc(ctx, id)
}
func (f *fakeStore) ListBundlesByTenant(ctx context.Context, tenantID string) ([]domain.PermissionBundle, error) {
	return f.listBundlesByTenantFunc(ctx, tenantID)
}
func (f *fakeStore) UpdatePermissionBundleActions(ctx context.Context, p domain.UpdatePermissionBundleActionsParams) (*domain.PermissionBundle, error) {
	return f.updateBundleActionsFunc(ctx, p)
}
func (f *fakeStore) CreateRolePermissionBundleLink(ctx context.Context, p domain.CreateRolePermissionBundleLinkParams) (*domain.RolePermissionBundleLink, error) {
	return f.createLinkFunc(ctx, p)
}
func (f *fakeStore) RemoveRolePermissionBundleLink(ctx context.Context, p domain.RemoveRolePermissionBundleLinkParams) error {
	return f.removeLinkFunc(ctx, p)
}
func (f *fakeStore) ListBundlesForRole(ctx context.Context, roleID string) ([]domain.PermissionBundle, error) {
	return f.listBundlesForRoleFunc(ctx, roleID)
}

type fakePublisher struct{}

func (f *fakePublisher) PublishRoleCreated(_ context.Context, _ string, _ domain.Role) error {
	return nil
}
func (f *fakePublisher) PublishRoleUpdated(_ context.Context, _ string, _ domain.Role) error {
	return nil
}
func (f *fakePublisher) PublishPermissionBundleUpdated(_ context.Context, _ string, _ domain.PermissionBundle) error {
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newRouter(store handler.AccessControlStore, pub handler.EventPublisher) *chi.Mux {
	r := chi.NewRouter()
	h := handler.New(store, pub, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func bodyJSON(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("bodyJSON: marshal failed: %v", err)
	}
	return bytes.NewBuffer(b)
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestCreateRole_Created201(t *testing.T) {
	role := &domain.Role{RoleID: "r1", TenantID: "t1", RoleCode: "ADMIN", RoleName: "Administrator", RoleScopeType: "TENANT"}
	store := &fakeStore{
		createRoleFunc: func(_ context.Context, _ domain.CreateRoleParams) (*domain.Role, bool, error) {
			return role, true, nil
		},
	}
	r := newRouter(store, &fakePublisher{})

	body := bodyJSON(t, map[string]string{
		"tenant_id": "t1", "role_code": "ADMIN", "role_name": "Administrator", "role_scope_type": "TENANT",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRole_IdempotentReplay200(t *testing.T) {
	role := &domain.Role{RoleID: "r1", TenantID: "t1", RoleCode: "ADMIN", RoleName: "Administrator", RoleScopeType: "TENANT"}
	store := &fakeStore{
		createRoleFunc: func(_ context.Context, _ domain.CreateRoleParams) (*domain.Role, bool, error) {
			return role, false, nil // already existed
		},
	}
	r := newRouter(store, &fakePublisher{})

	body := bodyJSON(t, map[string]string{
		"tenant_id": "t1", "role_code": "ADMIN", "role_name": "Administrator", "role_scope_type": "TENANT",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRole_MissingField400(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(store, &fakePublisher{})

	body := bodyJSON(t, map[string]string{"tenant_id": "t1"}) // missing role_code etc.
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestCreateRole_Conflict409(t *testing.T) {
	store := &fakeStore{
		createRoleFunc: func(_ context.Context, _ domain.CreateRoleParams) (*domain.Role, bool, error) {
			return nil, false, domain.ErrConflict
		},
	}
	r := newRouter(store, &fakePublisher{})

	body := bodyJSON(t, map[string]string{
		"tenant_id": "t1", "role_code": "ADMIN", "role_name": "Different Name", "role_scope_type": "TENANT",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w.Code)
	}
}

func TestGetRole_NotFound404(t *testing.T) {
	store := &fakeStore{
		findRoleByIDFunc: func(_ context.Context, _ string) (*domain.Role, error) {
			return nil, domain.ErrRoleNotFound
		},
	}
	r := newRouter(store, &fakePublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/roles/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestListRoles_MissingTenantID400(t *testing.T) {
	r := newRouter(&fakeStore{}, &fakePublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestDeactivateRole_NotFound404(t *testing.T) {
	store := &fakeStore{
		deactivateRoleFunc: func(_ context.Context, _ string) (*domain.Role, error) {
			return nil, domain.ErrRoleNotFound
		},
	}
	r := newRouter(store, &fakePublisher{})

	req := httptest.NewRequest(http.MethodPost, "/v1/roles/nonexistent/deactivate", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCreatePermissionBundle_Created201(t *testing.T) {
	bundle := &domain.PermissionBundle{
		PermissionBundleID: "b1", TenantID: "t1",
		BundleCode: "AP_OPS", BundleName: "AP Operations",
		PermittedActions: []string{"INVOICE_APPROVE", "PAYMENT_INITIATE"},
	}
	store := &fakeStore{
		createBundleFunc: func(_ context.Context, _ domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error) {
			return bundle, true, nil
		},
	}
	r := newRouter(store, &fakePublisher{})

	body := bodyJSON(t, map[string]any{
		"tenant_id": "t1", "bundle_code": "AP_OPS", "bundle_name": "AP Operations",
		"permitted_actions": []string{"INVOICE_APPROVE", "PAYMENT_INITIATE"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/permission-bundles", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateBundleActions_NotFound404(t *testing.T) {
	store := &fakeStore{
		updateBundleActionsFunc: func(_ context.Context, _ domain.UpdatePermissionBundleActionsParams) (*domain.PermissionBundle, error) {
			return nil, domain.ErrBundleNotFound
		},
	}
	r := newRouter(store, &fakePublisher{})

	body := bodyJSON(t, map[string]any{"permitted_actions": []string{"ACTION_X"}})
	req := httptest.NewRequest(http.MethodPut, "/v1/permission-bundles/nonexistent/actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestLinkBundle_RoleDeactivated422(t *testing.T) {
	store := &fakeStore{
		createLinkFunc: func(_ context.Context, _ domain.CreateRolePermissionBundleLinkParams) (*domain.RolePermissionBundleLink, error) {
			return nil, domain.ErrRoleDeactivated
		},
	}
	r := newRouter(store, &fakePublisher{})

	req := httptest.NewRequest(http.MethodPost, "/v1/roles/r1/permission-bundles/b1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", w.Code)
	}
}

func TestUnlinkBundle_NotFound404(t *testing.T) {
	store := &fakeStore{
		removeLinkFunc: func(_ context.Context, _ domain.RemoveRolePermissionBundleLinkParams) error {
			return domain.ErrLinkNotFound
		},
	}
	r := newRouter(store, &fakePublisher{})

	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/r1/permission-bundles/b1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}
