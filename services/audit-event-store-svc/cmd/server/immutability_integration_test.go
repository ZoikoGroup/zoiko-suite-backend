//go:build integration

// audit_events immutability (migration 000006) and payload-hash verification,
// against a real PostgreSQL with FORCE RLS and a NOSUPERUSER NOBYPASSRLS
// application role (same harness as rls_integration_test.go).
//
// Run with:
//
//	go test -v -tags=integration -timeout=250s ./cmd/server/
package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// tamperExec simulates a privileged rewrite of evidence: a superuser turns off
// trigger firing for one transaction (session_replication_role = replica) and
// then mutates audit_events. It exists only to prove the integrity checks
// notice such a rewrite; the application role can do none of this.
func tamperExec(ctx context.Context, admin *pgxpool.Pool, sql string, args ...any) (int64, error) {
	var affected int64
	err := pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, sql, args...)
		affected = tag.RowsAffected()
		return err
	})
	return affected, err
}

func storeEvent(t *testing.T, ctx context.Context, s *store.PgStore, id, tenant, payload string) *store.AuditEvent {
	t.Helper()
	e := &store.AuditEvent{
		EventID: id, EventType: "test.event", TenantID: tenant, LegalEntityID: "le-" + tenant,
		SourceService: "svc", SchemaVersion: "1.0", Payload: []byte(payload),
	}
	require.NoError(t, s.Store(ctx, e))
	return e
}

func rowCount(t *testing.T, ctx context.Context, admin *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, admin.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n))
	return n
}

func TestAuditEvents_DatabaseImmutability_RealPostgres(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_immutability_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_immutability_test")
	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	// Normal ingestion (INSERT under platform scope) works.
	first := storeEvent(t, ctx, pgStore, "imm-1", "tenant-a", `{"n":1}`)
	storeEvent(t, ctx, pgStore, "imm-2", "tenant-a", `{"n":2}`)
	storeEvent(t, ctx, pgStore, "imm-3", "tenant-b", `{"n":3}`)
	require.Equal(t, 3, rowCount(t, ctx, admin))
	assert.Equal(t, int64(1), first.SequenceNumber)

	t.Run("application role cannot UPDATE", func(t *testing.T) {
		// Tenant-scoped session, exactly what a tenant read path has.
		err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','tenant-a',true)`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE audit_events SET event_type='rewritten' WHERE event_id='imm-1'`)
			return err
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "append-only")
	})

	t.Run("application role under platform scope (the writer's own scope) cannot UPDATE or DELETE", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE audit_events SET payload='{"n":999}'::jsonb WHERE event_id='imm-2'`,
			`DELETE FROM audit_events WHERE event_id='imm-2'`,
		} {
			err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_scope','true',true)`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			require.Errorf(t, err, "must be rejected: %s", stmt)
			assert.Contains(t, err.Error(), "append-only")
		}
	})

	t.Run("application role cannot DELETE", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','tenant-a',true)`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE event_id='imm-1'`)
			return err
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "append-only")
	})

	t.Run("even a superuser cannot UPDATE, DELETE or TRUNCATE", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE audit_events SET payload_hash='x' WHERE event_id='imm-1'`,
			`DELETE FROM audit_events WHERE event_id='imm-1'`,
			`TRUNCATE audit_events`,
		} {
			_, err := admin.Exec(ctx, stmt)
			require.Errorf(t, err, "must be rejected: %s", stmt)
			assert.Contains(t, err.Error(), "append-only")
		}
	})

	t.Run("nothing changed and ingestion still works", func(t *testing.T) {
		assert.Equal(t, 3, rowCount(t, ctx, admin))
		again := storeEvent(t, ctx, pgStore, "imm-4", "tenant-a", `{"n":4}`)
		assert.Equal(t, int64(4), again.SequenceNumber, "chain continues after refused mutations")
		ok, n, err := pgStore.VerifyChain(ctx)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, int64(4), n)
	})

	t.Run("duplicate event_id does not create a second row", func(t *testing.T) {
		storeEvent(t, ctx, pgStore, "imm-4", "tenant-a", `{"n":4}`)
		assert.Equal(t, 4, rowCount(t, ctx, admin))
	})

	t.Run("tenant isolation is intact", func(t *testing.T) {
		res, err := pgStore.QueryEvents(ctx, domain.QueryEventsParams{TenantID: "tenant-b", Limit: 50})
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		assert.Equal(t, "imm-3", res.Events[0].EventID)
		other, err := pgStore.QueryEvents(ctx, domain.QueryEventsParams{TenantID: "tenant-zzz", Limit: 50})
		require.NoError(t, err)
		assert.Empty(t, other.Events)

		// Raw access as the app role with no tenant context still sees nothing.
		var n int
		require.NoError(t, appPool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&n))
		assert.Equal(t, 0, n, "FORCE RLS still applies to the application role")
	})
}

func TestAuditEvents_PayloadHashVerification_RealPostgres(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_payload_verify_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_payload_verify_test")
	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	// The hash covers the canonical JSONB text, so producer formatting (key
	// order, whitespace) does not change it.
	a := storeEvent(t, ctx, pgStore, "ph-a", "tenant-a", `{"b":1,"a":2}`)
	b := storeEvent(t, ctx, pgStore, "ph-b", "tenant-a", "{ \"a\": 2,\n  \"b\": 1 }")
	storeEvent(t, ctx, pgStore, "ph-c", "tenant-a", `{"c":3}`)
	assert.Equal(t, a.PayloadHash, b.PayloadHash, "same JSON value, same canonical hash")

	// Every stored row's hash can be recomputed from its stored payload.
	var mismatched int
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_events
		WHERE payload_hash_scheme = 'sha256-jsonb-text-v1'
		  AND payload_hash <> encode(sha256(convert_to(payload::text,'UTF8')),'hex')`).Scan(&mismatched))
	assert.Equal(t, 0, mismatched)
	var schemes int
	require.NoError(t, admin.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE payload_hash_scheme IS NULL`).Scan(&schemes))
	assert.Equal(t, 0, schemes, "new rows always carry a hash scheme")

	verify := func() (bool, int64) {
		ok, n, err := pgStore.VerifyChain(ctx)
		require.NoError(t, err)
		return ok, n
	}
	pageValid := func() bool {
		res, err := pgStore.QueryEvents(ctx, domain.QueryEventsParams{TenantID: "tenant-a", Limit: 50})
		require.NoError(t, err)
		return res.HashChainValid
	}
	ok, n := verify()
	assert.True(t, ok)
	assert.Equal(t, int64(3), n)
	assert.True(t, pageValid())

	t.Run("modified payload with the hash left unchanged is detected", func(t *testing.T) {
		_, err := tamperExec(ctx, admin, `UPDATE audit_events SET payload='{"a":999,"b":1}'::jsonb WHERE event_id='ph-b'`)
		require.NoError(t, err)
		ok, n := verify()
		assert.False(t, ok, "POST /v1/events/verify must fail when a stored payload no longer matches its hash")
		assert.Equal(t, int64(2), n, "reports the sequence number of the first failing row")
		assert.False(t, pageValid(), "hash_chain_valid must be false for a page containing the altered row")

		// An archive over the range is refused, and an old archive would DIVERGE.
		_, err = pgStore.CreateArchive(ctx, domain.CreateArchiveParams{FromSequence: 1, ToSequence: 3, TenantID: "tenant-a", CreatedByPrincipalID: "auditor"})
		require.ErrorIs(t, err, domain.ErrChainBroken)

		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET payload='{ "a": 2, "b": 1 }'::jsonb WHERE event_id='ph-b'`)
		require.NoError(t, err)
		ok, _ = verify()
		assert.True(t, ok, "restoring the same JSON value verifies again")
	})

	t.Run("modified stored hash is detected", func(t *testing.T) {
		_, err := tamperExec(ctx, admin, `UPDATE audit_events SET payload_hash='tampered' WHERE event_id='ph-c'`)
		require.NoError(t, err)
		ok, _ := verify()
		assert.False(t, ok)
		assert.False(t, pageValid())
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET payload_hash=encode(sha256(convert_to(payload::text,'UTF8')),'hex') WHERE event_id='ph-c'`)
		require.NoError(t, err)
		ok, _ = verify()
		assert.True(t, ok)
	})

	t.Run("broken previous-hash link is detected", func(t *testing.T) {
		var original string
		require.NoError(t, admin.QueryRow(ctx, `SELECT previous_event_hash FROM audit_events WHERE event_id='ph-c'`).Scan(&original))
		_, err := tamperExec(ctx, admin, `UPDATE audit_events SET previous_event_hash='broken' WHERE event_id='ph-c'`)
		require.NoError(t, err)
		ok, n := verify()
		assert.False(t, ok)
		assert.Equal(t, int64(3), n)
		assert.False(t, pageValid())
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET previous_event_hash=$1 WHERE event_id='ph-c'`, original)
		require.NoError(t, err)
		ok, _ = verify()
		assert.True(t, ok)
	})

	t.Run("broken sequence (a deleted row) is detected", func(t *testing.T) {
		// Re-insert-free simulation: renumber the last row to leave a gap.
		_, err := tamperExec(ctx, admin, `UPDATE audit_events SET sequence_number=5 WHERE event_id='ph-c'`)
		require.NoError(t, err)
		ok, n := verify()
		assert.False(t, ok)
		assert.Equal(t, int64(3), n, "the first missing sequence number")
		assert.False(t, pageValid())
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET sequence_number=3 WHERE event_id='ph-c'`)
		require.NoError(t, err)
		ok, _ = verify()
		assert.True(t, ok)
	})

	t.Run("legacy rows (no hash scheme) are checked for sequence and link only", func(t *testing.T) {
		// A row exactly as written before migration 000006: hash over the
		// producer's raw bytes, no scheme. Its payload cannot be re-derived,
		// so a payload edit on it is NOT detectable — the documented limit.
		_, err := tamperExec(ctx, admin, `UPDATE audit_events SET payload_hash_scheme=NULL WHERE event_id='ph-a'`)
		require.NoError(t, err)
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET payload='{"a":777}'::jsonb WHERE event_id='ph-a'`)
		require.NoError(t, err)
		ok, _ := verify()
		assert.True(t, ok, "a legacy row's payload is not verifiable; only its link is")
		// ...but its link still is:
		_, err = tamperExec(ctx, admin, `UPDATE audit_events SET payload_hash='tampered' WHERE event_id='ph-a'`)
		require.NoError(t, err)
		ok, _ = verify()
		assert.False(t, ok, "the next row's link to the legacy row's hash breaks")
	})
}
