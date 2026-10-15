package registry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/registry"
)

const tenant = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func serve(t *testing.T, code int, body string) *registry.HTTPClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/entities/e-1/status" || r.Header.Get("X-Tenant-Id") != tenant || r.Header.Get("X-Principal-Id") == "" {
			t.Errorf("request %s tenant=%q principal=%q", r.URL.Path, r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id"))
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return registry.NewHTTPClient(srv.URL, zap.NewNop())
}

func TestEntityInTenant(t *testing.T) {
	ctx := context.Background()
	m, err := serve(t, 200, `{"entity_id":"e-1","tenant_id":"`+tenant+`","entity_status":"active"}`).EntityInTenant(ctx, "e-1", tenant)
	if err != nil || !m.Found || m.Status != "ACTIVE" {
		t.Errorf("200: %+v %v", m, err)
	}
	m, err = serve(t, 404, `{}`).EntityInTenant(ctx, "e-1", tenant)
	if err != nil || m.Found {
		t.Errorf("404: %+v %v, want not found and no error", m, err)
	}
	for _, code := range []int{500, 503, 401} {
		if _, err := serve(t, code, `{}`).EntityInTenant(ctx, "e-1", tenant); !errors.Is(err, registry.ErrUnavailable) {
			t.Errorf("%d: err %v, want ErrUnavailable (fail closed)", code, err)
		}
	}
	// A 200 naming another tenant is not membership.
	if _, err := serve(t, 200, `{"tenant_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","entity_status":"ACTIVE"}`).EntityInTenant(ctx, "e-1", tenant); !errors.Is(err, registry.ErrUnavailable) {
		t.Errorf("foreign-tenant 200: err %v, want ErrUnavailable", err)
	}
}

func TestEntityInTenant_Unreachable(t *testing.T) {
	c := registry.NewHTTPClient("http://127.0.0.1:1", zap.NewNop())
	if _, err := c.EntityInTenant(context.Background(), "e-1", tenant); !errors.Is(err, registry.ErrUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
}
