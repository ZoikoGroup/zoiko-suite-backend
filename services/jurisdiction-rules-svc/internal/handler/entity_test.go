package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

type entityReader struct {
	tenant, entity string
	called         bool
}

func (e *entityReader) ListEntityJurisdictions(_ context.Context, tenantID, legalEntityID string, _ time.Time) ([]*domain.EntityJurisdictionApplicability, error) {
	e.called, e.tenant, e.entity = true, tenantID, legalEntityID
	return []*domain.EntityJurisdictionApplicability{{JurisdictionID: "j-1"}}, nil
}

// Entity jurisdiction mappings are tenant data: no tenant, no read.
func TestGetEntityJurisdictions_TenantScoped(t *testing.T) {
	rd := &entityReader{}
	r := chi.NewRouter()
	h := handler.New(&stubStore{}, permitAll(), &spyPublisher{}, testAuthzScopeID, zap.NewNop()).WithEntityApplicability(rd)
	handler.RegisterRoutes(r, h)

	rr := executeRequest(r, httptest.NewRequest(http.MethodGet, "/v1/entities/e-1/jurisdictions", nil))
	if rr.Code != http.StatusUnauthorized || rd.called {
		t.Fatalf("no X-Tenant-Id: want 401 without a store call, got %d (called=%v)", rr.Code, rd.called)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/entities/e-1/jurisdictions?effective_at=2026-03-01T00:00:00Z", nil)
	req.Header.Set("X-Tenant-Id", "t-1")
	if rr := executeRequest(r, req); rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rd.tenant != "t-1" || rd.entity != "e-1" {
		t.Fatalf("the read must be scoped to the caller's tenant and the path entity, got %q/%q", rd.tenant, rd.entity)
	}

	// Without the projection wired the route does not exist.
	r2 := chi.NewRouter()
	handler.RegisterRoutes(r2, handler.New(&stubStore{}, permitAll(), &spyPublisher{}, testAuthzScopeID, zap.NewNop()))
	req2 := httptest.NewRequest(http.MethodGet, "/v1/entities/e-1/jurisdictions", nil)
	req2.Header.Set("X-Tenant-Id", "t-1")
	if rr := executeRequest(r2, req2); rr.Code != http.StatusNotFound {
		t.Fatalf("unwired: want 404, got %d", rr.Code)
	}
}
