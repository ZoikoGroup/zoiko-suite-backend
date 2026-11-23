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

// The maker travels with a checker's authorization, so an own-object SoD rule
// in authorization-svc can apply; and two makers never share a cached answer.
func TestCheckAllowed_SendsResourceOwner(t *testing.T) {
	var owners []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		owner, _ := body["resource_owner_principal_id"].(string)
		owners = append(owners, owner)
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())
	ctx := WithResourceOwner(context.Background(), "author-1")
	if err := c.CheckAllowed(ctx, "approver", "le", ActionPolicyVersionApprove, "t"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckAllowed(WithResourceOwner(context.Background(), "author-2"), "approver", "le", ActionPolicyVersionApprove, "t"); err != nil {
		t.Fatal(err)
	}
	if len(owners) != 2 || owners[0] != "author-1" || owners[1] != "author-2" {
		t.Fatalf("each maker must be sent and not served from another's cache entry, got %v", owners)
	}
}

// An emergency check forwards the break-glass session, returns the decision
// basis so the caller can tell whether the grant came through that session, and
// is never cached: the session can end between two calls.
func TestCheckEmergency_ForwardsTheSessionReturnsTheBasisAndIsNeverCached(t *testing.T) {
	var hits int64
	var lastBody map[string]any
	var lastKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		lastKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED", DecisionBasis: "break_glass:session=sess-1:incident=INC-9"})
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())

	for i := 0; i < 3; i++ {
		basis, err := c.CheckEmergency(context.Background(), "p-1", "entity-1", "ACCOUNTING_POLICY_EMERGENCY_SUSPEND", "tenant-1", "sess-1")
		if err != nil || basis != "break_glass:session=sess-1:incident=INC-9" {
			t.Fatalf("call %d: basis %q err %v", i, basis, err)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("%d live calls for 3 emergency checks: an emergency decision must never be served from the cache", got)
	}
	if lastBody["break_glass_session_id"] != "sess-1" || lastBody["action_type"] != "ACCOUNTING_POLICY_EMERGENCY_SUSPEND" || lastBody["principal_id"] != "p-1" {
		t.Errorf("request = %v, want the session forwarded", lastBody)
	}
	if lastKey == "" || !contains(lastKey, "sess-1") {
		t.Errorf("Idempotency-Key = %q, want one decision per session", lastKey)
	}
}

// An ordinary check sends no session and is cached as before.
func TestCheckAllowed_SendsNoSession(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(authorizeResponse{DecisionOutcome: "GRANTED"})
	}))
	defer srv.Close()
	if err := NewHTTPClient(srv.URL, zap.NewNop()).CheckAllowed(context.Background(), "p", "e", "POLICY_READ", "t"); err != nil {
		t.Fatal(err)
	}
	if _, has := body["break_glass_session_id"]; has {
		t.Errorf("an ordinary check carried a session: %v", body)
	}
}

func TestCheckEmergency_DeniedAndUnavailable(t *testing.T) {
	var hits int64
	srv := newTestServer(t, "DENIED", &hits)
	defer srv.Close()
	basis, err := NewHTTPClient(srv.URL, zap.NewNop()).CheckEmergency(context.Background(), "p", "e", "A", "t", "s")
	if err != domain.ErrAuthorizationDenied || basis != "" {
		t.Errorf("denied: basis %q err %v", basis, err)
	}
	if _, err := NewHTTPClient("http://127.0.0.1:1", zap.NewNop()).CheckEmergency(context.Background(), "p", "e", "A", "t", "s"); err != domain.ErrAuthorizationServiceUnavailable {
		t.Errorf("unreachable: %v, want it to fail closed", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
