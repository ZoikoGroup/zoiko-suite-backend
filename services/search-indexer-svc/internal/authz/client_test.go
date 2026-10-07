package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/search-indexer-svc/internal/envelope"
)

func server(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func TestCheckAllowed_GrantedIsPermitted(t *testing.T) {
	s := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	})
	c := NewHTTPClient(s.URL, zap.NewNop())
	require.NoError(t, c.CheckAllowed(context.Background(), "p-1", "e-1", "SEARCH"))
}

func TestCheckAllowed_DeniedIsErrDenied(t *testing.T) {
	s := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision_outcome":"DENIED","decision_basis":"no_grant"}`))
	})
	c := NewHTTPClient(s.URL, zap.NewNop())
	assert.ErrorIs(t, c.CheckAllowed(context.Background(), "p-1", "e-1", "SEARCH"), ErrDenied)
}

// INV-07 / NP-04. Every way of not getting a decision fails CLOSED — the
// difference between them matters for the operator reading the evidence, not
// for whether the result is returned.
func TestCheckAllowed_FailsClosedOnEveryFailureMode(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		c := NewHTTPClient("http://127.0.0.1:1", zap.NewNop())
		assert.ErrorIs(t, c.CheckAllowed(context.Background(), "p", "e", "SEARCH"), ErrUnavailable)
	})

	t.Run("non-200", func(t *testing.T) {
		s := server(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		c := NewHTTPClient(s.URL, zap.NewNop())
		assert.ErrorIs(t, c.CheckAllowed(context.Background(), "p", "e", "SEARCH"), ErrUnavailable)
	})

	t.Run("unreadable body", func(t *testing.T) {
		s := server(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		})
		c := NewHTTPClient(s.URL, zap.NewNop())
		assert.ErrorIs(t, c.CheckAllowed(context.Background(), "p", "e", "SEARCH"), ErrUnavailable)
	})

	t.Run("unknown outcome", func(t *testing.T) {
		s := server(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"decision_outcome":"MAYBE"}`))
		})
		c := NewHTTPClient(s.URL, zap.NewNop())
		assert.ErrorIs(t, c.CheckAllowed(context.Background(), "p", "e", "SEARCH"), ErrDenied)
	})
}

// authorization-svc validates the same canonical envelope this service does
// and answers 400 without it. An unforwarded envelope would turn every gated
// call into a 503 that reads like an outage rather than a missing header.
func TestCheckAllowed_ForwardsTheCallersEnvelope(t *testing.T) {
	var got http.Header
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	})

	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		TenantID:       "tenant-1",
		ActorSubjectID: "principal-1",
		RequestID:      "req-1",
		CorrelationID:  "corr-1",
		SourceChannel:  svcenvelope.ChannelAPI,
	})

	c := NewHTTPClient(s.URL, zap.NewNop())
	require.NoError(t, c.CheckAllowed(ctx, "principal-1", "entity-1", "SEARCH"))

	assert.Equal(t, "tenant-1", got.Get("X-Tenant-Id"))
	assert.Equal(t, "principal-1", got.Get("X-Principal-Id"))
	assert.Equal(t, "entity-1", got.Get("X-Legal-Entity-Id"))
	assert.Equal(t, "req-1", got.Get("X-Request-Id"))
	assert.Equal(t, "corr-1", got.Get("X-Correlation-ID"))
	assert.Equal(t, "api", got.Get("X-Source-Channel"))
}

// One decision per (request, action, ENTITY). A search re-authorizes many
// entities under one request id, and collapsing them onto one idempotency key
// would record ONE decision for a whole page of results — losing exactly the
// per-resource audit trail TC-05 asks for.
func TestCheckAllowed_IdempotencyKeyIsPerEntity(t *testing.T) {
	keys := map[string]bool{}
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		keys[r.Header.Get("Idempotency-Key")] = true
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	})

	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		TenantID: "t-1", RequestID: "req-1", SourceChannel: svcenvelope.ChannelAPI,
	})
	c := NewHTTPClient(s.URL, zap.NewNop())
	require.NoError(t, c.CheckAllowed(ctx, "p-1", "entity-A", "SEARCH"))
	require.NoError(t, c.CheckAllowed(ctx, "p-1", "entity-B", "SEARCH"))

	assert.Len(t, keys, 2, "two entities under one request must be two decisions")
}

// The cache is keyed on the TENANT as well as the principal. Roles in
// authorization-svc are tenant-scoped, so keying without it stores an answer
// under a question it did not ask — and serves it to the wrong tenant for the
// rest of the TTL, in both directions.
func TestCheckAllowed_CacheIsTenantScoped(t *testing.T) {
	var calls int32
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("X-Tenant-Id") == "tenant-granted" {
			_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
			return
		}
		_, _ = w.Write([]byte(`{"decision_outcome":"DENIED"}`))
	})
	c := NewHTTPClient(s.URL, zap.NewNop())

	granted := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		TenantID: "tenant-granted", RequestID: "r", SourceChannel: svcenvelope.ChannelAPI})
	denied := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		TenantID: "tenant-denied", RequestID: "r", SourceChannel: svcenvelope.ChannelAPI})

	require.NoError(t, c.CheckAllowed(granted, "p-1", "e-1", "SEARCH"))
	// Same principal, same entity, same action — different tenant. Must NOT
	// be served the cached grant.
	assert.ErrorIs(t, c.CheckAllowed(denied, "p-1", "e-1", "SEARCH"), ErrDenied)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls),
		"a different tenant must produce a fresh decision, not a cache hit")
}

// A real decision is cached; an UNAVAILABLE outcome never is. Caching an
// outage would turn one transient failure into a standing answer for every
// subsequent caller on this instance, which defeats fail-closed.
func TestCheckAllowed_CachesDecisionsButNeverUnavailability(t *testing.T) {
	var calls int32
	s := server(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	})
	c := NewHTTPClient(s.URL, zap.NewNop())

	require.NoError(t, c.CheckAllowed(context.Background(), "p-1", "e-1", "SEARCH"))
	require.NoError(t, c.CheckAllowed(context.Background(), "p-1", "e-1", "SEARCH"))
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "a repeat within the TTL is a cache hit")

	down := NewHTTPClient("http://127.0.0.1:1", zap.NewNop())
	for i := 0; i < 3; i++ {
		assert.ErrorIs(t, down.CheckAllowed(context.Background(), "p", "e", "SEARCH"), ErrUnavailable)
	}
	down.mu.Lock()
	cached := len(down.cache)
	down.mu.Unlock()
	assert.Zero(t, cached, "an unavailable outcome must never enter the cache")
}

func TestDecisionCacheTTL_IsShort(t *testing.T) {
	// A search re-authorizes a page of results at once, so the cache absorbs
	// a burst inside one user action rather than spreading a decision across
	// a session. Long enough for the burst, short enough that a revocation is
	// not made materially worse by it.
	assert.LessOrEqual(t, decisionCacheTTL, 5*time.Second)
}

// A misconfigured deployment must NOT come up permitting everything. Refusing
// to build the stub outside local development means it does not come up at
// all, which is the only safe direction for a service whose job is deciding
// what a caller may see.
func TestNewClient_RefusesThePermitAllStubOutsideLocal(t *testing.T) {
	for _, url := range []string{"", "http://authorization-svc", "http://localhost"} {
		_, err := NewClient("production", url, zap.NewNop())
		require.Error(t, err, "url %q", url)
		assert.Contains(t, err.Error(), "refusing to start")
	}
}

func TestNewClient_AllowsTheStubInLocalDevelopment(t *testing.T) {
	c, err := NewClient("local", "", zap.NewNop())
	require.NoError(t, err)
	assert.IsType(t, &PermitAllClient{}, c)
}

func TestNewClient_BuildsTheRealClientWhenConfigured(t *testing.T) {
	c, err := NewClient("production", "http://authorization-svc:8089", zap.NewNop())
	require.NoError(t, err)
	assert.IsType(t, &HTTPClient{}, c)
}
