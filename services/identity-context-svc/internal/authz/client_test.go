package authz_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/authz"
	"zoiko.io/identity-context-svc/internal/domain"
)

// The authorization client guards every mutating route on this service and had
// no tests at all.
//
// Everything here is about one property: it FAILS CLOSED, and it distinguishes
// "you may not" from "we could not tell". Those two are different facts — one
// is the control working, the other is a governance-plane outage — and a client
// that collapses them reports an outage as a permissions problem, sending the
// reader to an RBAC grant that was never the issue.

func serving(t *testing.T, status int, body string) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		assert.Equal(t, "/v1/authorize", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func check(c authz.Client) error {
	return c.CheckAllowed(context.Background(), "p-1", "entity-1", "IDENTITY_PRINCIPAL_READ", "tenant-a")
}

// ── The decision itself ──────────────────────────────────────────────────────

// TestGrantedAndDeniedBothArriveAs200.
//
// The status reflects "the evaluation succeeded", not the outcome, so the body
// must always be read. A client that branched on the status alone would treat
// every DENIED as a grant.
func TestGrantedIsPermitted(t *testing.T) {
	srv, _ := serving(t, http.StatusOK, `{"decision_outcome":"GRANTED","access_decision_id":"ad-1"}`)
	c := authz.NewHTTPClient(srv.URL, zap.NewNop())

	require.NoError(t, check(c))
}

func TestDeniedIsRefusedAndDistinguishable(t *testing.T) {
	srv, _ := serving(t, http.StatusOK,
		`{"decision_outcome":"DENIED","decision_basis":"no matching grant","access_decision_id":"ad-2"}`)
	c := authz.NewHTTPClient(srv.URL, zap.NewNop())

	err := check(c)
	require.ErrorIs(t, err, domain.ErrAuthorizationDenied)
	assert.NotErrorIs(t, err, domain.ErrAuthorizationServiceUnavailable,
		"a refusal and an outage must never be the same error")
}

// TestAnyNonGrantedOutcomeIsADenial guards the future.
//
// A later authorization-svc adding a third outcome — STEP_UP_REQUIRED, say —
// must not have it read as a grant. Defaulting an unknown verdict to permitted
// is how a new deny state ships as a no-op.
func TestAnyNonGrantedOutcomeIsADenial(t *testing.T) {
	for _, outcome := range []string{"DENIED", "STEP_UP_REQUIRED", "INDETERMINATE", ""} {
		srv, _ := serving(t, http.StatusOK, `{"decision_outcome":"`+outcome+`"}`)
		c := authz.NewHTTPClient(srv.URL, zap.NewNop())

		assert.ErrorIs(t, check(c), domain.ErrAuthorizationDenied,
			"outcome %q must not be read as a grant", outcome)
	}
}

// ── Fail closed ──────────────────────────────────────────────────────────────

func TestUnreachableServiceFailsClosed(t *testing.T) {
	c := authz.NewHTTPClient("http://127.0.0.1:1", zap.NewNop())

	err := check(c)
	require.ErrorIs(t, err, domain.ErrAuthorizationServiceUnavailable)
	assert.NotErrorIs(t, err, domain.ErrAuthorizationDenied,
		"an outage is not a decision — the caller reports 503, not 403")
}

func TestNon200FailsClosed(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, // envelope_incomplete
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusNotFound,
	} {
		srv, _ := serving(t, status, `{"decision_outcome":"GRANTED"}`)
		c := authz.NewHTTPClient(srv.URL, zap.NewNop())

		assert.ErrorIs(t, check(c), domain.ErrAuthorizationServiceUnavailable,
			"status %d must not be read as a grant even with a permissive body", status)
	}
}

func TestUnreadableBodyFailsClosed(t *testing.T) {
	srv, _ := serving(t, http.StatusOK, `not json`)
	c := authz.NewHTTPClient(srv.URL, zap.NewNop())

	assert.ErrorIs(t, check(c), domain.ErrAuthorizationServiceUnavailable)
}

// ── Caching ──────────────────────────────────────────────────────────────────

// TestDecisionsAreCached keeps the authorize round trip off every request.
func TestGrantedDecisionsAreCached(t *testing.T) {
	srv, calls := serving(t, http.StatusOK, `{"decision_outcome":"GRANTED"}`)
	c := authz.NewHTTPClient(srv.URL, zap.NewNop())

	for i := 0; i < 5; i++ {
		require.NoError(t, check(c))
	}
	assert.EqualValues(t, 1, atomic.LoadInt64(calls), "a repeated identical check should not re-ask")
}

func TestDeniedDecisionsAreCachedToo(t *testing.T) {
	srv, calls := serving(t, http.StatusOK, `{"decision_outcome":"DENIED"}`)
	c := authz.NewHTTPClient(srv.URL, zap.NewNop())

	for i := 0; i < 5; i++ {
		assert.ErrorIs(t, check(c), domain.ErrAuthorizationDenied)
	}
	assert.EqualValues(t, 1, atomic.LoadInt64(calls))
}

// TestOutagesAreNeverCached is the important half of the caching behaviour.
//
// Caching an unavailable outcome would extend a momentary blip into a fixed
// window during which every affected caller is refused — turning a one-request
// failure into a multi-second outage, and one that persists after the service
// has already recovered.
func TestUnavailableOutcomesAreNeverCached(t *testing.T) {
	var calls int64
	var fail atomic.Bool
	fail.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()

	c := authz.NewHTTPClient(srv.URL, zap.NewNop())

	require.ErrorIs(t, check(c), domain.ErrAuthorizationServiceUnavailable)
	require.ErrorIs(t, check(c), domain.ErrAuthorizationServiceUnavailable)
	assert.EqualValues(t, 2, atomic.LoadInt64(&calls), "an outage must be re-asked, not remembered")

	// Once it recovers, the very next call sees the grant.
	fail.Store(false)
	require.NoError(t, check(c))
}

// TestCacheKeyIncludesEveryDimension.
//
// A key that dropped the action would let a grant for one action authorize a
// different one; dropping the tenant would let a grant in one tenant authorize
// another. Both are silent.
func TestCacheDistinguishesEveryDimension(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()

	c := authz.NewHTTPClient(srv.URL, zap.NewNop())
	ctx := context.Background()

	require.NoError(t, c.CheckAllowed(ctx, "p-1", "e-1", "ACTION_A", "tenant-a"))
	require.NoError(t, c.CheckAllowed(ctx, "p-2", "e-1", "ACTION_A", "tenant-a")) // principal
	require.NoError(t, c.CheckAllowed(ctx, "p-1", "e-2", "ACTION_A", "tenant-a")) // entity
	require.NoError(t, c.CheckAllowed(ctx, "p-1", "e-1", "ACTION_B", "tenant-a")) // action
	require.NoError(t, c.CheckAllowed(ctx, "p-1", "e-1", "ACTION_A", "tenant-b")) // tenant

	assert.EqualValues(t, 5, atomic.LoadInt64(&calls),
		"each dimension must produce a distinct cache key")
}

// ── Wiring guard ─────────────────────────────────────────────────────────────

// TestStubIsRefusedInProduction.
//
// A governance control that is silently absent is worse than one that is loudly
// broken, because only the second gets fixed.
func TestPermitAllStubIsRefusedOutsideDevelopment(t *testing.T) {
	for _, env := range []string{"production", "staging"} {
		_, err := authz.NewClient(env, "", zap.NewNop())
		require.Error(t, err, "%s must not run without a real authorization-svc", env)

		_, err = authz.NewClient(env, "http://localhost:8089", zap.NewNop())
		require.Error(t, err, "%s must not accept a localhost placeholder", env)

		_, err = authz.NewClient(env, "http://authz.example.com", zap.NewNop())
		require.Error(t, err, "%s must not accept an example.com placeholder", env)
	}
}

// TestNonProductionURLsAreRefusedOutsideDevelopment.
//
// The guard this exercises used to be an exact-match list of two strings, so
// anything merely SHAPED like a placeholder — a loopback port, a documentation
// domain — read as a real authorization service and the deployment came up.
//
// The rule is deliberately a positive test for provably-local addresses. An
// unrecognised host is treated as real, because refusing one is a refusal to
// boot in production, which is worse than the failure being prevented.
func TestNonProductionURLsAreRefusedOutsideDevelopment(t *testing.T) {
	refused := []string{
		"http://localhost:8089",
		"http://127.0.0.1:8089",
		"http://[::1]:8089",
		"http://0.0.0.0:8089",
		"http://authz.example.com",
		"https://authz.example.org",
		"http://authz.example.net",
		"http://authz.invalid",
		"http://authz.test",
		"authorization-svc:8089", // no scheme — addresses nothing
	}
	for _, env := range []string{"production", "staging", "PRODUCTION"} {
		for _, url := range refused {
			_, err := authz.NewClient(env, url, zap.NewNop())
			require.Error(t, err, "%s must refuse %q", env, url)
		}
	}
}

// TestDeployedURLsAreAcceptedInProduction.
//
// The other half, and the one that costs an outage if it regresses: a guard
// that refuses real addresses is a guard nobody can deploy behind. A private
// RFC 1918 address is a perfectly ordinary way to reach authorization-svc.
func TestDeployedURLsAreAcceptedInProduction(t *testing.T) {
	for _, url := range []string{
		"http://authorization-svc:8089",
		"http://authorization-svc.governance.svc.cluster.local:8089",
		"https://authz.internal.zoiko.io",
		"http://10.0.3.7:8089",
	} {
		c, err := authz.NewClient("production", url, zap.NewNop())
		require.NoError(t, err, "production must accept %q", url)
		require.NotNil(t, c)
	}
}

// TestLoopbackIsARealClientInDevelopment.
//
// Loopback is refused in production but must NOT collapse into the permit-all
// stub in development: a locally-run authorization-svc is addressed exactly
// this way, and silently ignoring it would make local authorization testing
// impossible while looking like it worked.
func TestLoopbackIsARealClientInDevelopment(t *testing.T) {
	c, err := authz.NewClient("development", "http://localhost:8089", zap.NewNop())
	require.NoError(t, err)
	_, isStub := c.(*authz.PermitAllClient)
	assert.False(t, isStub, "a configured loopback URL in development is a real client, not the stub")
}

func TestRealClientIsBuiltForAProductionURL(t *testing.T) {
	c, err := authz.NewClient("production", "http://authorization-svc:8089", zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestStubPermitsInDevelopment(t *testing.T) {
	c, err := authz.NewClient("development", "", zap.NewNop())
	require.NoError(t, err)
	assert.NoError(t, check(c), "local development gets a permit-all stub")
}

func TestConfiguredURLWinsOverTheStubInDevelopment(t *testing.T) {
	srv, _ := serving(t, http.StatusOK, `{"decision_outcome":"DENIED"}`)
	c, err := authz.NewClient("development", srv.URL, zap.NewNop())
	require.NoError(t, err)

	assert.ErrorIs(t, check(c), domain.ErrAuthorizationDenied,
		"a configured URL must be used even in development — the stub is a fallback, not a preference")
}

var _ authz.Client = (*authz.HTTPClient)(nil)
