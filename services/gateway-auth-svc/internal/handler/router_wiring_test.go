package handler_test

// These tests exercise the router main.go actually serves, rather than calling
// Handler.Verify directly as the rest of this file's tests do.
//
// That distinction is the whole point. Every pre-existing test invoked the
// handler in isolation and with GET, so the middleware stack around it was
// never covered — and a middleware that refused /verify on every non-GET method
// sat in main.go with a fully green suite. Traefik's ForwardAuth replays the
// client's original method onto /verify, so POST/PUT/PATCH/DELETE are the
// common case in production and were exactly the case nothing tested.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"zoiko.io/gateway-auth-svc/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/router"
)

// serveThroughRouter runs one request through the real middleware stack.
func serveThroughRouter(t *testing.T, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	h, key, cfg := newTestEnv(t)
	// nil metrics: this suite exercises the middleware stack, not the
	// Prometheus wiring, and a real registry would be registered twice in one
	// test binary.
	r := router.New(h, jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL), nil)

	req := httptest.NewRequest(method, path, nil)
	if headers == nil {
		req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	}
	for k, v := range headers {
		if v == "@token" {
			v = "Bearer " + mintEnvelope(t, key, testKid, validClaims())
		}
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The ForwardAuth endpoint cannot demand the headers it exists to produce.
//
// Traefik calls /verify with the client's method and the client's headers. The
// client has no X-Tenant-Id or X-Principal-Id to send — the gateway derives
// both from the signed token — so requiring them as envelope input refuses
// every state-changing request on the platform before its JWT is ever checked.
func TestRouter_VerifyIsExemptFromEnvelopeContract(t *testing.T) {
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodGet,
	} {
		t.Run(method, func(t *testing.T) {
			rec := serveThroughRouter(t, method, "/verify", nil)

			require.Equal(t, http.StatusOK, rec.Code,
				"%s /verify must reach the handler; the envelope middleware must not gate it", method)
			assert.Equal(t, "principal-xyz", rec.Header().Get("X-Principal-Id"))
			assert.Equal(t, "tenant-abc", rec.Header().Get("X-Tenant-Id"))
			assert.Empty(t, rec.Header().Get("X-Envelope-Contract"),
				"an exempt path must not be reported as violating the contract")
		})
	}
}

// The exemption is a named path, not a disabled middleware. A bad token still
// fails at the handler, which is where authentication belongs.
func TestRouter_VerifyStillRejectsBadTokenThroughRouter(t *testing.T) {
	rec := serveThroughRouter(t, http.MethodPost, "/verify", map[string]string{
		"Authorization": "Bearer not-a-jwt",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRouter_ProbesRemainOpen(t *testing.T) {
	rec := serveThroughRouter(t, http.MethodGet, "/healthz", map[string]string{})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestRouter_ExposesMetrics proves /metrics is actually mounted and carries
// this service's own series.
//
// The only test in this binary that builds a real telemetry.Metrics:
// NewMetrics registers its collectors on the default Prometheus registry with
// MustRegister, so a second call panics. Every other test passes nil, which is
// why they need no registry at all.
func TestRouter_ExposesMetrics(t *testing.T) {
	h, _, cfg := newTestEnv(t)
	m := telemetry.NewMetrics("gateway-auth-svc")
	r := router.New(h, jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL), m)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "/metrics must be reachable — this service had no metrics endpoint at all")
	body := rec.Body.String()

	// Pre-initialised at zero, so an alert written against them evaluates from
	// startup instead of staying silent until the first occurrence. For the
	// security outcomes that is the whole point: "no series" and "no spoofing
	// attempts" must not look identical.
	for _, want := range []string{
		"gateway_auth_verify_decisions_total",
		"gateway_auth_jwks_errors_total",
		"gateway_auth_tenant_context_total",
		"gateway_auth_carta_decisions_total",
		"readiness_up",
	} {
		assert.Contains(t, body, want)
	}
	assert.Contains(t, body, `outcome="tenant_hostname_mismatch"`,
		"the spoofing outcome must exist as a series before it ever fires")
}
