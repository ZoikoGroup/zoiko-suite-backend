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
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", ownRoleTenant)
		w := httptest.NewRecorder()
		cachedRouter(s).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s through cache.Store: want 200, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}
