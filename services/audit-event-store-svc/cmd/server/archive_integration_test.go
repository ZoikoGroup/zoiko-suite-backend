//go:build integration

// AUD-10 archive/verify integration tests — same embedded-postgres harness
// as rls_integration_test.go (startRLSPostgres/appRolePool, same package).
//
// Run with:
//
//	go test -v -tags=integration -timeout=120s ./cmd/server/
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/store"
)

func seedEvents(t *testing.T, ctx context.Context, pgStore *store.PgStore, n int) []*store.AuditEvent {
	t.Helper()
	events := make([]*store.AuditEvent, 0, n)
	for i := 0; i < n; i++ {
		e := &store.AuditEvent{
			EventID: "archtest-" + string(rune('a'+i)), EventType: "test.event",
			TenantID: "tenant-a", LegalEntityID: "le-a", SourceService: "svc", SchemaVersion: "1.0",
			Payload: []byte(`{"n":` + string(rune('0'+i)) + `}`),
		}
		require.NoError(t, pgStore.Store(ctx, e))
		events = append(events, e)
	}
	return events
}

// TestArchiveStore_CreateArchive_HappyPathAndBrokenChainDetection is the
// real proof of the ErrChainBroken mechanism: a hash-link broken inside the
// requested range must refuse to write an archive, and once the link is
// restored the same range archives cleanly.
func TestArchiveStore_CreateArchive_HappyPathAndBrokenChainDetection(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_archive_create_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_archive_create_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	events := seedEvents(t, ctx, pgStore, 5)

	archive, err := pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 5, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5), archive.EventCount)
	assert.Equal(t, events[0].PayloadHash, archive.FirstEventHash)
	assert.Equal(t, events[4].PayloadHash, archive.LastEventHash)
	assert.NotEmpty(t, archive.ArchiveDigest)

	// Break the chain: corrupt sequence 3's previous_event_hash so it no
	// longer equals sequence 2's payload_hash. audit_events is immutable at the
	// database level (migration 000006), so the negative control simulates a
	// privileged rewrite: a superuser disables trigger firing for one
	// transaction (tamperExec) and edits the row. A superuser also bypasses
	// audit_events' FORCE RLS, which is fine here — this is test tampering, not
	// an RLS assertion (that is rls_integration_test.go's job).
	var original string
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT previous_event_hash FROM audit_events WHERE sequence_number=3`).Scan(&original))
	_, err = tamperExec(ctx, admin,
		`UPDATE audit_events SET previous_event_hash='deliberately-corrupted' WHERE sequence_number=3`)
	require.NoError(t, err)

	_, err = pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 5, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1",
	})
	require.ErrorIs(t, err, domain.ErrChainBroken,
		"CreateArchive must refuse to archive a range with a broken hash link")

	// Restore and confirm the mechanism is not just permanently broken.
	_, err = tamperExec(ctx, admin,
		`UPDATE audit_events SET previous_event_hash=$1 WHERE sequence_number=3`, original)
	require.NoError(t, err)
	_, err = pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 5, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err, "restoring the link must let the same range archive cleanly again")
}

// TestArchiveStore_VerifyArchive_DetectsPostArchiveTampering is the real
// proof of VerifyArchive's own reason to exist: an archive that was valid
// at creation time must be reported DIVERGED once the underlying chain is
// tampered with afterwards, and VERIFIED again once restored — each call
// recorded permanently via ListVerifications.
func TestArchiveStore_VerifyArchive_DetectsPostArchiveTampering(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_archive_verify_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_archive_verify_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	seedEvents(t, ctx, pgStore, 4)

	archive, err := pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 4, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	v1, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: archive.ArchiveID, TenantID: "tenant-a", VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.VerificationVerified, v1.Result)
	assert.Nil(t, v1.FirstDivergentSequence)

	var original string
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT payload_hash FROM audit_events WHERE sequence_number=2`).Scan(&original))
	_, err = tamperExec(ctx, admin,
		`UPDATE audit_events SET payload_hash='tampered-hash' WHERE sequence_number=2`)
	require.NoError(t, err)

	v2, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: archive.ArchiveID, TenantID: "tenant-a", VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.VerificationDiverged, v2.Result,
		"tampering with an already-archived row's payload_hash must be detected on re-verification")
	require.NotNil(t, v2.FirstDivergentSequence)

	_, err = tamperExec(ctx, admin,
		`UPDATE audit_events SET payload_hash=$1 WHERE sequence_number=2`, original)
	require.NoError(t, err)
	v3, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: archive.ArchiveID, TenantID: "tenant-a", VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.VerificationVerified, v3.Result, "restoring the row must verify clean again")

	history, err := pgStore.ListVerifications(ctx, archive.ArchiveID, "tenant-a")
	require.NoError(t, err)
	assert.Len(t, history, 3, "every verification attempt — VERIFIED or DIVERGED — must be permanently recorded")
}

// TestPgStore_Archive_IsAppendOnly is the real proof (with a genuine
// negative control, not just a positive assertion) of the
// reject_archive_mutation trigger installed by migration 000004.
//
// MIGRATION 000005 interacts with this test in a way that is easy to get
// wrong, so the two concerns are established INDEPENDENTLY and in order:
//
//  1. Tenant isolation first. Migration 000005 FORCEs row-level security on
//     audit_archives, so a session with no app.tenant_id — or with another
//     tenant's — sees ZERO rows. The row must be proven visible to its
//     owner BEFORE any mutation is attempted, because an UPDATE that matches
//     no rows touches no rows and therefore never reaches a BEFORE UPDATE
//     trigger. That silent false positive ("got no error" is then reported
//     as a trigger failure, or worse, an unscoped UPDATE is reported as a
//     trigger success) is exactly what this test previously suffered from.
//  2. Append-only integrity second, on a dedicated tenant-scoped
//     connection: the UPDATE/DELETE now really reach the row, so any
//     refusal they produce can only come from the trigger — asserted
//     explicitly against the trigger's own error text, never merely
//     "some error happened".
//
// Neither RLS nor FORCE RLS nor the trigger is disabled, bypassed, or
// weakened to make this pass; the tenant context the production store sets
// (archive_store.go setTenantScope) is the context this test sets too.
func TestPgStore_Archive_IsAppendOnly(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_archive_immutable_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_archive_immutable_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	seedEvents(t, ctx, pgStore, 2)
	archive, err := pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 2, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	// A single acquired connection, not a pooled Exec: set_config is a
	// session-scoped setting, so it must land on the same session that runs
	// every statement below (pgxpool.Exec may use a different connection per
	// call, which would quietly discard the tenant scope).
	conn, err := appPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	// ── 1. Tenant isolation, proved independently of the trigger ─────────────
	// Another tenant's scope sees nothing at all.
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-b', false)")
	require.NoError(t, err)
	var foreign int64
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM audit_archives`).Scan(&foreign))
	assert.Zero(t, foreign,
		"tenant-b must see no audit_archives rows — migration 000005 FORCE RLS is inert if this fails")

	// The owning tenant sees exactly its own row. Everything from here on is
	// therefore an assertion about the APPEND-ONLY TRIGGER, not about RLS
	// filtering the row out of existence.
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-a', false)")
	require.NoError(t, err)
	var visible int64
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_archives WHERE archive_id=$1`, archive.ArchiveID).Scan(&visible))
	require.Equal(t, int64(1), visible,
		"tenant-a must see its own archive row before any mutation is attempted — without this the UPDATE below touches no rows and never reaches the trigger")

	var original string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT archive_digest FROM audit_archives WHERE archive_id=$1`, archive.ArchiveID).Scan(&original))
	require.Equal(t, archive.ArchiveDigest, original, "precondition: the stored digest is the one CreateArchive wrote")

	// ── 2. Append-only integrity, now reaching the real row ──────────────────
	_, err = conn.Exec(ctx,
		`UPDATE audit_archives SET archive_digest='forged' WHERE archive_id=$1`, archive.ArchiveID)
	require.Error(t, err, "expected the append-only trigger to refuse mutating an archived record")
	assert.Contains(t, err.Error(), "audit_archives is append-only",
		"the refusal must be raised by migration 000004's reject_archive_mutation trigger — a plain 'no rows affected' would mean RLS, not the trigger, did the work")

	_, err = conn.Exec(ctx, `DELETE FROM audit_archives WHERE archive_id=$1`, archive.ArchiveID)
	require.Error(t, err, "expected archives to never be deletable")
	assert.Contains(t, err.Error(), "audit_archives is append-only",
		"DELETE must be refused by the trigger, for the same reason as UPDATE")

	// The record is byte-for-byte what it was: neither refused statement
	// partially applied.
	var unchanged string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT archive_digest FROM audit_archives WHERE archive_id=$1`, archive.ArchiveID).Scan(&unchanged))
	assert.Equal(t, original, unchanged, "a refused UPDATE must leave the archive's claim intact")

	// ── Negative control ─────────────────────────────────────────────────────
	// Disable the trigger on the admin (superuser) connection and confirm the
	// SAME tenant-scoped connection can now mutate the row: this is what
	// proves the refusals above were the trigger and nothing else (not RLS,
	// not a missing row, not a privilege error). RLS itself stays enabled and
	// forced throughout — only the trigger is toggled, and only temporarily.
	_, err = admin.Exec(ctx, `ALTER TABLE audit_archives DISABLE TRIGGER trg_reject_archive_update`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`UPDATE audit_archives SET archive_digest='forged-while-disabled' WHERE archive_id=$1`, archive.ArchiveID)
	require.NoError(t, err, "with the trigger disabled the same UPDATE must succeed — proving the trigger was the real mechanism")
	var mutated string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT archive_digest FROM audit_archives WHERE archive_id=$1`, archive.ArchiveID).Scan(&mutated))
	assert.Equal(t, "forged-while-disabled", mutated,
		"the only mutation that ever lands must be the one made while the trigger was disabled")

	_, err = admin.Exec(ctx, `ALTER TABLE audit_archives ENABLE TRIGGER trg_reject_archive_update`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`UPDATE audit_archives SET archive_digest='forged-again' WHERE archive_id=$1`, archive.ArchiveID)
	require.Error(t, err, "re-enabling the trigger must restore the refusal")
	assert.Contains(t, err.Error(), "audit_archives is append-only")

	// Both protections still independently asserted after the toggle: RLS is
	// still enforced (tenant-b still sees nothing) and the trigger is still
	// armed (digest still the disabled-window value).
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-b', false)")
	require.NoError(t, err)
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM audit_archives`).Scan(&foreign))
	assert.Zero(t, foreign, "FORCE RLS must still be in force after the trigger toggle")
}

// TestArchiveStore_TenantIsolation_RealRLS proves migration 000005 end to end
// against real PostgreSQL: both archive tables are ENABLE + FORCE row-level
// security, the application role is genuinely subject to the policy, and the
// store's tenant-scoped methods refuse to read, verify, or enumerate another
// tenant's archive. A missing tenant fails closed at the store boundary too.
func TestArchiveStore_TenantIsolation_RealRLS(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_archive_tenant_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_archive_tenant_test")

	// The migration must really have enabled AND forced RLS on both archive
	// tables - otherwise the assertions below would pass merely because no
	// policy is active.
	for _, table := range []string{"audit_archives", "audit_archive_verifications"} {
		var enabled, forced bool
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced))
		require.Truef(t, enabled, "%s must ENABLE row level security (migration 000005)", table)
		require.Truef(t, forced, "%s must FORCE row level security (migration 000005)", table)
	}

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	seedEvents(t, ctx, pgStore, 3) // tenant-a

	arch, err := pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 3, TenantID: "tenant-a", CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "tenant-a", arch.TenantID, "archive creation must persist the caller's tenant")

	// Owner can read, verify, and read its own history.
	got, err := pgStore.GetArchive(ctx, arch.ArchiveID, "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, "tenant-a", got.TenantID)

	v, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: arch.ArchiveID, TenantID: "tenant-a", VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "tenant-a", v.TenantID, "a verification must be recorded under the verifying tenant")

	history, err := pgStore.ListVerifications(ctx, arch.ArchiveID, "tenant-a")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "tenant-a", history[0].TenantID)

	// Another tenant sees nothing - the same ErrArchiveNotFound as a genuinely
	// missing archive, so no cross-tenant existence signal leaks.
	_, err = pgStore.GetArchive(ctx, arch.ArchiveID, "tenant-b")
	assert.ErrorIs(t, err, domain.ErrArchiveNotFound, "tenant B must not read tenant A's archive")

	_, err = pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: arch.ArchiveID, TenantID: "tenant-b", VerifiedByPrincipalID: "attacker",
	})
	assert.ErrorIs(t, err, domain.ErrArchiveNotFound, "tenant B must not verify tenant A's archive")

	_, err = pgStore.ListVerifications(ctx, arch.ArchiveID, "tenant-b")
	assert.ErrorIs(t, err, domain.ErrArchiveNotFound, "tenant B must not read tenant A's verification history")

	// The refused cross-tenant verification must not have written a row.
	var verificationCount int64
	require.NoError(t, admin.QueryRow(ctx, `SELECT COUNT(*) FROM audit_archive_verifications`).Scan(&verificationCount))
	assert.Equal(t, int64(1), verificationCount, "a refused cross-tenant verification must leave no row")

	// Missing tenant fails closed at the store boundary (defense in depth
	// behind the handler's 401).
	_, err = pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 3, CreatedByPrincipalID: "auditor-1",
	})
	assert.ErrorIs(t, err, domain.ErrTenantRequired)
	_, err = pgStore.GetArchive(ctx, arch.ArchiveID, "")
	assert.ErrorIs(t, err, domain.ErrTenantRequired)
	_, err = pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{ArchiveID: arch.ArchiveID, VerifiedByPrincipalID: "x"})
	assert.ErrorIs(t, err, domain.ErrTenantRequired)
	_, err = pgStore.ListVerifications(ctx, arch.ArchiveID, "")
	assert.ErrorIs(t, err, domain.ErrTenantRequired)

	// RLS is active against the application role, not merely declared: with no
	// app.tenant_id set, a raw read returns 0 rows even though one exists;
	// scoped to tenant-a it returns exactly that row.
	conn, err := appPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	var unscoped int64
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM audit_archives`).Scan(&unscoped))
	assert.Zero(t, unscoped, "no tenant scope must yield no archive rows under FORCE RLS")

	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-a', false)")
	require.NoError(t, err)
	var scoped int64
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM audit_archives`).Scan(&scoped))
	assert.Equal(t, int64(1), scoped, "the owning tenant must see exactly its own archive")
}
