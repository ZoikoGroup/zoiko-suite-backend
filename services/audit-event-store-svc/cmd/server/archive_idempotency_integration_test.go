//go:build integration

// POST /v1/archives idempotency against a real PostgreSQL (migration 000007),
// run as the NOSUPERUSER NOBYPASSRLS application role so row-level security is
// genuinely in force. Same embedded-postgres harness as the other integration
// tests in this package.
//
//	go test -v -tags=integration -timeout=240s -run Idempotency ./cmd/server/
package main

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/store"
)

func idemParams(tenant, key, fingerprint string, from, to int64) domain.CreateArchiveParams {
	return domain.CreateArchiveParams{
		FromSequence: from, ToSequence: to, TenantID: tenant, CreatedByPrincipalID: "auditor-1",
		Idem: &domain.Idempotency{Key: key, Endpoint: "POST /v1/archives", Fingerprint: fingerprint},
	}
}

func TestArchiveIdempotency_AgainstRealPostgres(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_archive_idem_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_archive_idem_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)
	seedEvents(t, ctx, pgStore, 6)

	archives := func() int {
		var n int
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM audit_archives`).Scan(&n))
		return n
	}
	keyRows := func(key string) int {
		var n int
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE idempotency_key=$1`, key).Scan(&n))
		return n
	}

	t.Run("first request then exact replay return one archive", func(t *testing.T) {
		before := archives()
		p1 := idemParams("tenant-a", "k-replay", "fp-1", 1, 4)
		first, err := pgStore.CreateArchive(ctx, p1)
		require.NoError(t, err)
		assert.False(t, p1.Idem.Replayed)

		p2 := idemParams("tenant-a", "k-replay", "fp-1", 1, 4)
		second, err := pgStore.CreateArchive(ctx, p2)
		require.NoError(t, err)
		assert.True(t, p2.Idem.Replayed)
		assert.Equal(t, first.ArchiveID, second.ArchiveID)
		assert.Equal(t, first.ArchiveDigest, second.ArchiveDigest)
		assert.Equal(t, first.CreatedAt.UTC(), second.CreatedAt.UTC(), "the original record, not a new one")
		assert.Equal(t, before+1, archives(), "exactly one new archive")

		var status int
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT response_status FROM idempotency_keys WHERE idempotency_key='k-replay'`).Scan(&status))
		assert.Equal(t, 201, status, "the claim carries the recorded response")
	})

	t.Run("concurrent duplicates create exactly one archive", func(t *testing.T) {
		before := archives()
		const n = 12
		ids := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				a, err := pgStore.CreateArchive(ctx, idemParams("tenant-a", "k-race", "fp-race", 1, 5))
				errs[i] = err
				if a != nil {
					ids[i] = a.ArchiveID
				}
			}(i)
		}
		close(start)
		wg.Wait()
		for i := 0; i < n; i++ {
			require.NoError(t, errs[i])
			assert.Equal(t, ids[0], ids[i], "every concurrent caller gets the one original archive")
		}
		assert.Equal(t, before+1, archives(), "12 concurrent identical requests must create one archive")
		assert.Equal(t, 1, keyRows("k-race"))
	})

	t.Run("same key with a different request is refused and returns nothing else", func(t *testing.T) {
		first, err := pgStore.CreateArchive(ctx, idemParams("tenant-a", "k-mismatch", "fp-A", 1, 3))
		require.NoError(t, err)
		before := archives()

		got, err := pgStore.CreateArchive(ctx, idemParams("tenant-a", "k-mismatch", "fp-B", 1, 6))
		require.ErrorIs(t, err, domain.ErrIdempotencyMismatch)
		assert.Nil(t, got)
		assert.Equal(t, before, archives(), "a mismatched reuse writes nothing")
		_ = first
	})

	t.Run("another tenant's key is independent", func(t *testing.T) {
		a, err := pgStore.CreateArchive(ctx, idemParams("tenant-a", "k-shared", "fp-s", 1, 3))
		require.NoError(t, err)
		pb := idemParams("tenant-b", "k-shared", "fp-s", 1, 3)
		b, err := pgStore.CreateArchive(ctx, pb)
		require.NoError(t, err)
		assert.False(t, pb.Idem.Replayed, "tenant-b must not replay tenant-a's key")
		assert.NotEqual(t, a.ArchiveID, b.ArchiveID)
		assert.Equal(t, "tenant-b", b.TenantID)
		assert.Equal(t, 2, keyRows("k-shared"))
	})

	t.Run("a failed archive leaves no claim behind and the key stays usable", func(t *testing.T) {
		// Empty range: refused, and the claim made before the range was read
		// is rolled back with it.
		_, err := pgStore.CreateArchive(ctx, idemParams("tenant-a", "k-rollback", "fp-r", 500, 600))
		require.ErrorIs(t, err, domain.ErrEmptyRange)
		assert.Equal(t, 0, keyRows("k-rollback"), "a failed request must not leave a replay record")

		// Broken chain: same.
		var original string
		require.NoError(t, admin.QueryRow(ctx, `SELECT previous_event_hash FROM audit_events WHERE sequence_number=3`).Scan(&original))
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET previous_event_hash='corrupted' WHERE sequence_number=3`)
		require.NoError(t, err)
		_, err = pgStore.CreateArchive(ctx, idemParams("tenant-a", "k-rollback2", "fp-r2", 1, 5))
		require.ErrorIs(t, err, domain.ErrChainBroken)
		assert.Equal(t, 0, keyRows("k-rollback2"))

		// Repair, retry the SAME key and request: it now succeeds as a first use,
		// not a replay of the earlier failure.
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET previous_event_hash=$1 WHERE sequence_number=3`, original)
		require.NoError(t, err)
		p := idemParams("tenant-a", "k-rollback2", "fp-r2", 1, 5)
		a, err := pgStore.CreateArchive(ctx, p)
		require.NoError(t, err)
		assert.False(t, p.Idem.Replayed)
		assert.NotEmpty(t, a.ArchiveID)
		assert.Equal(t, 1, keyRows("k-rollback2"))
	})

	t.Run("no key behaves as before and writes no claim", func(t *testing.T) {
		before, claims := archives(), 0
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&claims))
		for i := 0; i < 2; i++ {
			_, err := pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
				FromSequence: 1, ToSequence: 2, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1"})
			require.NoError(t, err)
		}
		assert.Equal(t, before+2, archives())
		var after int
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&after))
		assert.Equal(t, claims, after)
	})

	t.Run("idempotency_keys is tenant-isolated under forced row-level security", func(t *testing.T) {
		var enabled, forced bool
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname='idempotency_keys'`).Scan(&enabled, &forced))
		assert.True(t, enabled)
		assert.True(t, forced)

		visible := func(tenant string) int {
			conn, err := appPool.Acquire(ctx)
			require.NoError(t, err)
			defer conn.Release()
			if tenant != "" {
				_, err = conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenant)
				require.NoError(t, err)
			}
			var n int
			require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE idempotency_key='k-shared'`).Scan(&n))
			return n
		}
		assert.Equal(t, 0, visible(""), "no tenant context: no keys visible")
		assert.Equal(t, 0, visible("tenant-z"), "an unrelated tenant sees none")
		assert.Equal(t, 1, visible("tenant-a"), "a tenant sees only its own")
		assert.Equal(t, 1, visible("tenant-b"))
	})
}
