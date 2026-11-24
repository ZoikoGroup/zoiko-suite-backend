package upstream_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/upstream"
)

// Tests for the readiness probe added to close the long-standing
// "TODO: add upstream Tenant Registry liveness check" in internal/health.
//
// It matters more than it looks: the registry is a fail-closed dependency of
// Dimension 2, so a registry outage means EVERY resolution returns 503 while
// this service's own probe reports healthy and the pod stays in the load
// balancer. The probe should say what the request path already knows.

func clientFor(url string) *upstream.RegistryClient {
	return upstream.NewRegistryClient(&config.Config{TenantRegistryURL: url}, zap.NewNop())
}

// TestPingAsksForThePathTheRegistryActuallyServes.
//
// This assertion USED TO READ "/health", and it passed, and it was wrong.
// tenant-entity-registry-svc serves /healthz and /readyz — /health is a 404
// there — so the probe reported a healthy registry as unreachable, /health on
// this service returned 503 forever, and a Kubernetes pod would never have
// gone ready. A stub that answers whatever it is asked cannot catch that; the
// only thing holding the two services together is this string, matched against
// what the running registry serves and against tenant-svc's own compose
// healthcheck.
//
// If this assertion is ever "fixed" to make a test pass, check the registry
// first.
func TestPingAsksForThePathTheRegistryActuallyServes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/healthz", r.URL.Path,
			"tenant-entity-registry-svc serves /healthz; /health is a 404 there")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, clientFor(srv.URL).Ping(context.Background()))
}

// TestPingDoesNotCascadeADegradedRegistry.
//
// A registry answering 503 from its OWN degraded probe is still REACHABLE.
// Treating that as a failure here would take the entire identity tier out of
// rotation because one downstream dependency of a downstream dependency is
// slow — which turns a partial degradation into a total outage.
//
// This is the case worth getting right, and the easy thing to get wrong.
func TestPingDoesNotCascadeADegradedRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := clientFor(srv.URL).Ping(context.Background())

	// It DOES report a failure — the registry said it is not serving — but the
	// distinction being pinned is that this is decided by the status code, not
	// by reachability, and a 2xx from a degraded-but-serving registry passes.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestPingAcceptsAny2xx(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, 299} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		require.NoError(t, clientFor(srv.URL).Ping(context.Background()),
			"status %d should count as reachable", status)
		srv.Close()
	}
}

func TestPingFailsWhenTheRegistryIsUnreachable(t *testing.T) {
	// A port nothing is listening on.
	err := clientFor("http://127.0.0.1:1").Ping(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable")
}

func TestPingToleratesATrailingSlashInTheConfiguredURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "http://registry//health" would 404 on most routers, and the
		// resulting alert would read as "the registry has no health endpoint"
		// rather than "somebody put a slash in an environment variable".
		assert.Equal(t, "/healthz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, clientFor(srv.URL+"/").Ping(context.Background()))
}

func TestPingRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// A readiness probe that can outlive its own deadline is one that holds a
	// connection open per scrape while the thing it is probing is hung.
	require.Error(t, clientFor(srv.URL).Ping(ctx))
}

func TestNameIsStableForTheChecksMap(t *testing.T) {
	// The dashboard, the alert rules and the runbook all key off this string.
	assert.Equal(t, "tenant_registry", clientFor("http://x").Name())
}

// tenant-entity-registry-svc's identity gate (require mode) admits a caller
// with no envelope only when it arrives over mTLS as a registered peer. A
// context resolve is what mints the envelope, so it has none to send: with
// WithMTLS every registry call must leave on the mTLS URL with the client that
// presents this service's certificate, and none may fall back to the plain URL
// (which the gate answers 401, failing every sign-in).
func TestWithMTLSSendsEveryRegistryCallToTheMTLSURL(t *testing.T) {
	plainHits := 0
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer plain.Close()

	var paths []string
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		assert.NotNil(t, r.TLS, "the registry call must arrive over TLS")
		if r.URL.Path == "/v1/tenants/11111111-1111-1111-1111-111111111111" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"lifecycle_state":"ACTIVE"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer secure.Close()

	c := clientFor(plain.URL).WithMTLS(secure.URL, secure.Client())

	active, err := c.IsTenantActive(context.Background(), "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	assert.True(t, active)
	require.NoError(t, c.Ping(context.Background()))

	assert.Equal(t, []string{"/v1/tenants/11111111-1111-1111-1111-111111111111", "/healthz"}, paths)
	assert.Zero(t, plainHits, "no registry call may go to the plain URL once mTLS is configured")
}

func TestWithoutMTLSTheRegistryIsCalledOnThePlainURL(t *testing.T) {
	hits := 0
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()

	require.NoError(t, clientFor(plain.URL).Ping(context.Background()))
	assert.Equal(t, 1, hits)
}
