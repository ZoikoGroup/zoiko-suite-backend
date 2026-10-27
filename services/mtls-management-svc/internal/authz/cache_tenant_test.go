package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	svcenvelope "zoiko.io/mtls-management-svc/internal/envelope"
)

// Found live on 13 Oct 2026: the decision cache was keyed without the tenant,
// so tenant A's GRANTED answered the same question asked under tenant B.
func TestCheckAllowed_CacheIsPerTenant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-Id") != "tenant-a" {
			w.WriteHeader(http.StatusForbidden) // tenant_scope_mismatch
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	inTenant := func(tenant string) context.Context {
		return svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{TenantID: tenant})
	}
	if err := c.CheckAllowed(inTenant("tenant-a"), "p", "le", "MTLS_CERT_READ"); err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	if err := c.CheckAllowed(inTenant("tenant-b"), "p", "le", "MTLS_CERT_READ"); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("tenant B reused tenant A's cached GRANTED: err = %v", err)
	}
}
