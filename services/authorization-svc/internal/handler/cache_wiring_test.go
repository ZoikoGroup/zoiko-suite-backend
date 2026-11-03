package handler_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/cache"
	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Production hands the handler cache.Store, not the PgStore. The handler finds
// its optional store capabilities by type assertion, so a capability the cache
// does not forward is invisible there and the route answers 503 — which no
// test running the handler over the bare stub can see. These run the routes
// that depend on such capabilities through the real wrapper.
type cacheableStub struct{ *stubStore }

// The consumer-side writes cache.Inner also carries; unused by these routes.
func (cacheableStub) FindDelegationCeilings(context.Context, string, string, string, string) ([]domain.DelegationCeiling, error) {
	return nil, nil
}
func (cacheableStub) ProjectDelegation(context.Context, domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error) {
	return nil, nil
}
func (cacheableStub) ProjectPrincipalStatus(context.Context, domain.ProjectPrincipalStatusParams) (*domain.PrincipalStatusProjection, error) {
	return nil, nil
}
func (cacheableStub) ProjectEntityStatus(context.Context, domain.ProjectEntityStatusParams) error {
	return nil
}
func (cacheableStub) RevokeProjectedDelegation(context.Context, string, string, string, int64) (*domain.DelegatedAuthority, error) {
	return nil, nil
}

func cachedRouter(s *stubStore) chi.Router {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(cache.New(cacheableStub{s}, time.Second, zap.NewNop()), &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop()))
	return r
}

func TestCacheWiring_CapabilityRoutesWork(t *testing.T) {
	for _, path := range []string{
		"/v1/admin/permission-bundles/b-1/reactivate", // FindPermissionBundleByID
		"/v1/admin/role-assignments/a-1/revoke",       // FindRoleAssignmentByID
	} {
		s := &stubStore{
			rbacActions:   stubAdminActions, // the cache reads grants through the scoped lookup
			bundleActive:  &domain.PermissionBundle{PermissionBundleID: "b-1", RoleID: "r-1"},
			revokedAssign: &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1"},
			// The revoke route checks If-Match against this read (000038).
			findAssignment: &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1", Version: 1},
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
		req.Header.Set("If-Match", `"v1"`) // API-3
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", ownRoleTenant)
		w := httptest.NewRecorder()
		cachedRouter(s).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s through cache.Store: want 200, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}

// suspendableInner forwards the ORG-06 suspension capabilities, as PgStore does.
type suspendableInner struct {
	cacheableStub
	calls []string
}

func (s *suspendableInner) SuspendRoleAssignment(_ context.Context, id, _, _, _ string) (*domain.PrincipalRoleAssignment, error) {
	s.calls = append(s.calls, "suspend")
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, ApprovalStatus: domain.ApprovalSuspended, Version: 2}, nil
}
func (s *suspendableInner) ResumeRoleAssignment(_ context.Context, id, _, _, _ string) (*domain.PrincipalRoleAssignment, error) {
	s.calls = append(s.calls, "resume")
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, ApprovalStatus: domain.ApprovalApproved, Version: 3}, nil
}
func (s *suspendableInner) ListAssignmentSuspensions(context.Context, string, string) ([]domain.AssignmentSuspension, error) {
	s.calls = append(s.calls, "suspensions")
	return nil, nil
}
func (s *suspendableInner) QueryAssignmentsAsOf(context.Context, domain.AssignmentsAsOfQuery) ([]domain.PrincipalRoleAssignment, error) {
	s.calls = append(s.calls, "as-of")
	return nil, nil
}

// Suspend, resume, the suspension list and the as-of read reach the store
// through cache.Store; a capability it did not forward would answer 503.
func TestCacheWiring_SuspensionRoutesReachTheStore(t *testing.T) {
	inner := &suspendableInner{cacheableStub: cacheableStub{&stubStore{
		rbacActions:    stubAdminActions,
		findAssignment: &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1", Version: 1},
	}}}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(cache.New(inner, time.Second, zap.NewNop()), &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop()))
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{"reason_code":"LEAVE"}`},
		{http.MethodPost, "/v1/admin/role-assignments/a-1/resume", `{}`},
		{http.MethodGet, "/v1/admin/role-assignments/a-1/suspensions", ``},
		{http.MethodGet, "/v1/admin/role-assignments/as-of", ``},
	} {
		req := httptest.NewRequest(c.method, c.path, bytes.NewBufferString(c.body))
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", ownRoleTenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s %s through cache.Store: want 200, got %d: %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if want := []string{"suspend", "resume", "suspensions", "as-of"}; len(inner.calls) != len(want) {
		t.Fatalf("store calls %v, want %v", inner.calls, want)
	}
}
