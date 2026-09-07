package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
)

func newTestServer(t *testing.T, outcome string, hits *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: outcome})
	}))
}

// TestCheckAllowed_CachesGrantedDecision proves a repeat identical check
// within decisionCacheTTL never reaches authorization-svc — this is the
// Doc 05 §6.5 local-policy-cache mitigation.
func TestCheckAllowed_CachesGrantedDecision(t *testing.T) {
	var hits int64
	srv := newTestServer(t, "GRANTED", &hits)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())

	for i := 0; i < 5; i++ {
		if err := c.CheckAllowed(context.Background(), "principal-1", "entity-1", "POLICY_CREATE", "tenant-1"); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("expected exactly 1 live call to authorization-svc, got %d", got)
	}
}

// TestCheckAllowed_CachesDeniedDecision proves DENIED is cached too, not
// just GRANTED — a repeatedly-denied caller shouldn't keep hammering
// authorization-svc either.
func TestCheckAllowed_CachesDeniedDecision(t *testing.T) {
	var hits int64
	srv := newTestServer(t, "DENIED", &hits)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())

	for i := 0; i < 3; i++ {
		err := c.CheckAllowed(context.Background(), "principal-2", "entity-1", "POLICY_CREATE", "tenant-1")
		if err != domain.ErrAuthorizationDenied {
			t.Fatalf("call %d: expected ErrAuthorizationDenied, got %v", i, err)
		}
	}

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("expected exactly 1 live call to authorization-svc, got %d", got)
	}
}

// TestCheckAllowed_DoesNotCacheUnavailable proves a failed/unreachable call
// is retried live every time — caching an outage would turn one transient
// failure into a standing fail-closed lockout for the whole cache TTL
// window, and worse, could mask a recovery.
func TestCheckAllowed_DoesNotCacheUnavailable(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:1", zap.NewNop()) // nothing listens here

	for i := 0; i < 3; i++ {
		if err := c.CheckAllowed(context.Background(), "principal-3", "entity-1", "POLICY_CREATE", "tenant-1"); err != domain.ErrAuthorizationServiceUnavailable {
			t.Fatalf("call %d: expected ErrAuthorizationServiceUnavailable, got %v", i, err)
		}
	}
}

// TestCheckAllowed_DistinctKeysAreNotConflated proves the cache key
// actually incorporates principal/entity/action — a grant for one
// combination must never leak into a different one.
func TestCheckAllowed_DistinctKeysAreNotConflated(t *testing.T) {
	var hits int64
	srv := newTestServer(t, "GRANTED", &hits)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())

	_ = c.CheckAllowed(context.Background(), "principal-4", "entity-1", "POLICY_CREATE", "tenant-1")
	_ = c.CheckAllowed(context.Background(), "principal-4", "entity-1", "POLICY_VERSION_CREATE", "tenant-1")
	_ = c.CheckAllowed(context.Background(), "principal-4", "entity-2", "POLICY_CREATE", "tenant-1")
	_ = c.CheckAllowed(context.Background(), "principal-5", "entity-1", "POLICY_CREATE", "tenant-1")

	if got := atomic.LoadInt64(&hits); got != 4 {
		t.Fatalf("expected 4 distinct live calls for 4 distinct keys, got %d", got)
	}
}

// TestCheckAllowed_ExpiresAfterTTL proves a stale decision is not reused
// forever — this is the "stale decision risk is bounded" requirement.
func TestCheckAllowed_ExpiresAfterTTL(t *testing.T) {
	var hits int64
	srv := newTestServer(t, "GRANTED", &hits)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())
	_ = c.CheckAllowed(context.Background(), "principal-6", "entity-1", "POLICY_CREATE", "tenant-1")

	// Force expiry without sleeping decisionCacheTTL in a unit test.
	c.cacheMu.Lock()
	for k, v := range c.cache {
		v.expiresAt = time.Now().Add(-time.Second)
		c.cache[k] = v
	}
	c.cacheMu.Unlock()

	_ = c.CheckAllowed(context.Background(), "principal-6", "entity-1", "POLICY_CREATE", "tenant-1")

	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("expected the second call after expiry to re-hit authorization-svc, got %d live calls", got)
	}
}
