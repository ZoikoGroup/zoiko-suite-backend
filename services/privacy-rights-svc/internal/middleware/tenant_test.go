package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The middleware is the fail-closed gate in front of every handler: whatever
// the handlers do for themselves, the next handler must NEVER run without a
// principal and a tenant, and the refusal follows the documented order.

func gate(t *testing.T, headers map[string]string) (code int, errCode string, ranNext bool, ctxTenant, ctxCorr string) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranNext = true
		ctxTenant = TenantFromContext(r.Context())
		ctxCorr = CorrelationFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	TenantContext()(next).ServeHTTP(w, req)
	if !ranNext {
		var m map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("refusal is not the JSON envelope: %q", w.Body.String())
		}
		errCode = m["error_code"]
		if m["error"] == "" {
			t.Fatalf("refusal carries no message: %q", w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("refusal Content-Type = %q", ct)
		}
	}
	return w.Code, errCode, ranNext, ctxTenant, ctxCorr
}

func TestTenantContext_RefusalOrderAndFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"neither principal nor tenant", nil, "AUTHENTICATION_FAILED"},
		{"whitespace principal, no tenant", map[string]string{"X-Principal-Id": "  "}, "AUTHENTICATION_FAILED"},
		{"no principal, valid tenant", map[string]string{"X-Tenant-Id": "t1"}, "AUTHENTICATION_FAILED"},
		{"valid principal, no tenant", map[string]string{"X-Principal-Id": "p1"}, "TENANT_SCOPE_REQUIRED"},
		{"valid principal, whitespace tenant", map[string]string{"X-Principal-Id": "p1", "X-Tenant-Id": "   "}, "TENANT_SCOPE_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, errCode, ran, _, _ := gate(t, tc.headers)
			if ran {
				t.Fatal("the next handler ran without a verified principal and tenant")
			}
			if code != http.StatusUnauthorized || errCode != tc.want {
				t.Fatalf("got %d %s, want 401 %s", code, errCode, tc.want)
			}
		})
	}
}

func TestTenantContext_PassesVerifiedContextThrough(t *testing.T) {
	code, _, ran, tenant, corr := gate(t, map[string]string{
		"X-Principal-Id": "p1", "X-Tenant-Id": " tenant-9 ", "X-Correlation-ID": "corr-1",
	})
	if !ran || code != http.StatusNoContent {
		t.Fatalf("a request with principal and tenant must reach the handler, got %d (ran=%v)", code, ran)
	}
	if tenant != "tenant-9" {
		t.Errorf("tenant on context = %q, want the trimmed header", tenant)
	}
	if corr != "corr-1" {
		t.Errorf("correlation on context = %q", corr)
	}
	// No correlation header: nothing on the context (reads).
	if _, _, _, _, corr = gate(t, map[string]string{"X-Principal-Id": "p1", "X-Tenant-Id": "t"}); corr != "" {
		t.Errorf("correlation on context = %q, want empty", corr)
	}
}
