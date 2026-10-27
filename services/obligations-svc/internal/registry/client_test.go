package registry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/registry"
)

func TestCheckEntity(t *testing.T) {
	var gotPath, gotTenant, gotAuth string
	status, body := http.StatusOK, `{"entity_id":"le-1","tenant_id":"T1","entity_status":"ACTIVE"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant, gotAuth = r.URL.EscapedPath(), r.Header.Get("X-Tenant-Id"), r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := registry.New(srv.URL, zap.NewNop())
	ctx := idenv.WithIdentity(context.Background(), &idenv.Identity{Basis: idenv.BasisEnvelope, Token: "env-token", PrincipalID: "p1", TenantID: "T1"})

	if err := c.CheckEntity(ctx, "T1", "le-1"); err != nil {
		t.Fatalf("an entity in the tenant: %v", err)
	}
	if gotPath != "/v1/entities/le-1/status" || gotTenant != "T1" || gotAuth != "Bearer env-token" {
		t.Fatalf("request path=%q tenant=%q auth=%q; the caller's envelope must be forwarded", gotPath, gotTenant, gotAuth)
	}
	if err := c.CheckEntity(ctx, "T1", "../tenants/x"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/entities/..%2Ftenants%2Fx/status" {
		t.Fatalf("an id must stay one path segment, got %q", gotPath)
	}

	body = `{"entity_id":"le-1","tenant_id":"T2"}`
	if err := c.CheckEntity(ctx, "T1", "le-1"); !errors.Is(err, domain.ErrLegalEntityNotFound) {
		t.Fatalf("another tenant's entity: %v", err)
	}
	status = http.StatusNotFound
	if err := c.CheckEntity(ctx, "T1", "le-1"); !errors.Is(err, domain.ErrLegalEntityNotFound) {
		t.Fatalf("404: %v", err)
	}
	for _, s := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		status = s
		if err := c.CheckEntity(ctx, "T1", "le-1"); !errors.Is(err, domain.ErrRegistryUnavailable) {
			t.Fatalf("%d must fail closed as unavailable, got %v", s, err)
		}
	}
	srv.Close()
	if err := c.CheckEntity(ctx, "T1", "le-1"); !errors.Is(err, domain.ErrRegistryUnavailable) {
		t.Fatalf("unreachable registry: %v", err)
	}
}
