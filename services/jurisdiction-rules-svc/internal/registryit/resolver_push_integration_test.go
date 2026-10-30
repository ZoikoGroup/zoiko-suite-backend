//go:build integration

// ZS-JUR-001 s23 and s29: a withdrawal, block or unblock reaches every resolver replica at once (Postgres LISTEN/NOTIFY, migration 000025),
// not only when its cache expires. Two resolvers share one database and both cache for an hour: the one with the listener follows the change,
// the control without it keeps serving the stale answer, which is exactly what the listener exists to prevent.
package registryit_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
)

// listeningEnv is a resolver with a long cache TTL that drops its cache when the database says something changed.
type listeningEnv struct {
	*env
	connects int32
	res      *resolver.Resolver
}

func newEnvListening(t *testing.T, signer domain.Signer, ttl time.Duration) *listeningEnv {
	t.Helper()
	pub := &spyPub{}
	auth := &recordingAuthz{}
	res, err := resolver.New(st, resolver.Config{EligibleStatuses: []string{"RELEASED"}, CacheTTL: ttl})
	require.NoError(t, err)
	le := &listeningEnv{res: res}
	lctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go st.ListenResolverChanges(lctx, func(payload string) {
		if payload == "connected" {
			atomic.AddInt32(&le.connects, 1)
		}
		res.Invalidate()
	})
	r := chi.NewRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithCertificationPolicy(1).WithResolver(res)
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	le.env = &env{srv: r, pub: pub, auth: auth}
	return le
}

func (le *listeningEnv) waitConnects(t *testing.T, n int32) {
	t.Helper()
	require.Eventually(t, func() bool { return atomic.LoadInt32(&le.connects) >= n }, 10*time.Second, 50*time.Millisecond, "the listener connects")
}

func resolveCode(e *env, c *certFixture) int {
	code, _ := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	return code
}

func TestResolverPush_EveryReplicaFollowsAChangeAtOnce(t *testing.T) {
	var le *listeningEnv
	c := signedReadyEnv(t, func(s domain.Signer) *env {
		le = newEnvListening(t, s, time.Hour)
		return le.env
	})
	c.certifyVersion(t, c.version)
	le.waitConnects(t, 1)
	setStatus(t, c, c.version, "RELEASED")

	// The control replica has the same cache TTL and no listener.
	control := newEnvResolver(nil, []string{"RELEASED"}, time.Hour)
	require.Equal(t, 200, resolveCode(le.env, c), "both replicas resolve and cache")
	require.Equal(t, 200, resolveCode(control, c))

	// An emergency block: the replica with the listener stops serving at once, the control keeps serving its cached answer.
	code, r := c.e.do("POST", c.fixture.path(c.version, "/block"), "ic-1", map[string]any{"reason": "suspected wrong rate"})
	mustStatus(t, 200, code, r)
	require.Eventually(t, func() bool { return resolveCode(le.env, c) != 200 }, 5*time.Second, 50*time.Millisecond, "the listening replica follows the block")
	assert.Equal(t, 200, resolveCode(control, c), "without the listener, a replica serves the blocked version until its cache expires")

	// Unblocking is followed the same way.
	code, r = c.e.do("POST", c.fixture.path(c.version, "/unblock"), "ic-1", map[string]any{"reason": "incident review complete"})
	mustStatus(t, 200, code, r)
	require.Eventually(t, func() bool { return resolveCode(le.env, c) == 200 }, 5*time.Second, 50*time.Millisecond, "and the unblock")

	// A lost connection is recovered: the listener reconnects (and invalidates on reconnect, in case it missed something).
	before := atomic.LoadInt32(&le.connects)
	var killed int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE query LIKE 'LISTEN jur_resolver_invalidate%' AND pid <> pg_backend_pid()`).Scan(&killed))
	require.GreaterOrEqual(t, killed, 1, "the listener's connection was found and terminated")
	le.waitConnects(t, before+1)

	// After the reconnect a withdrawal still reaches the replica.
	require.Equal(t, 200, resolveCode(le.env, c))
	code, r = c.e.do("POST", c.fixture.path(c.version, "/withdraw"), "ic-1", map[string]any{"reason": "withdrawn after incident", "evidence_ref": "INC-9"})
	mustStatus(t, 200, code, r)
	require.Eventually(t, func() bool { return resolveCode(le.env, c) != 200 }, 5*time.Second, 50*time.Millisecond, "the withdrawal after a reconnect")
}

func TestResolverPush_ARolledBackChangeIsNeverAnnounced(t *testing.T) {
	var le *listeningEnv
	c := signedReadyEnv(t, func(s domain.Signer) *env {
		le = newEnvListening(t, s, time.Hour)
		return le.env
	})
	c.certifyVersion(t, c.version)
	le.waitConnects(t, 1)
	setStatus(t, c, c.version, "RELEASED")
	require.Equal(t, 200, resolveCode(le.env, c))
	hitsBefore, missesBefore, _ := listenerStats(t, le)

	// A change that is rolled back must not make replicas drop their caches: NOTIFY is delivered only on COMMIT.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO deployment_regions (region_code, created_by_principal_id) VALUES ('push-rollback-test', 'seed') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE jurisdiction_pack_versions v SET updated_at = NOW() FROM jurisdiction_packs p WHERE p.pack_id=v.pack_id AND p.pack_ref=$1`, c.packRef)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 200, resolveCode(le.env, c))
	hits, misses, _ := listenerStats(t, le)
	assert.Equal(t, missesBefore, misses, "a rolled-back transaction announced nothing, so the cached answer was reused")
	assert.Greater(t, hits, hitsBefore)
}

func listenerStats(t *testing.T, le *listeningEnv) (hits, misses, refreshes int) {
	t.Helper()
	return le.res.Stats()
}
