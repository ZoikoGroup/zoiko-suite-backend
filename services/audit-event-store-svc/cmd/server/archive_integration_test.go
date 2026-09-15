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
		FromSequence: 1, ToSequence: 5, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5), archive.EventCount)
	assert.Equal(t, events[0].PayloadHash, archive.FirstEventHash)
	assert.Equal(t, events[4].PayloadHash, archive.LastEventHash)
	assert.NotEmpty(t, archive.ArchiveDigest)

	// Break the chain: corrupt sequence 3's previous_event_hash so it no
	// longer equals sequence 2's payload_hash. audit_events carries no
	// DB-level immutability trigger (see migration 000001/000002's own
	// header — immutability here is application discipline, not a
	// constraint), so a direct UPDATE is the legitimate way to simulate
	// tampering for this negative control.
	// Tamper via the admin (superuser) connection — a straight superuser
	// bypasses audit_events' FORCE RLS entirely, which is exactly what's
	// wanted here: this is test setup/tampering, not an RLS assertion (that
	// is rls_integration_test.go's job).
	var original string
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT previous_event_hash FROM audit_events WHERE sequence_number=3`).Scan(&original))
	_, err = admin.Exec(ctx,
		`UPDATE audit_events SET previous_event_hash='deliberately-corrupted' WHERE sequence_number=3`)
	require.NoError(t, err)

	_, err = pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 5, CreatedByPrincipalID: "auditor-1",
	})
	require.ErrorIs(t, err, domain.ErrChainBroken,
		"CreateArchive must refuse to archive a range with a broken hash link")

	// Restore and confirm the mechanism is not just permanently broken.
	_, err = admin.Exec(ctx,
		`UPDATE audit_events SET previous_event_hash=$1 WHERE sequence_number=3`, original)
	require.NoError(t, err)
	_, err = pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 5, CreatedByPrincipalID: "auditor-1",
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
		FromSequence: 1, ToSequence: 4, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	v1, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: archive.ArchiveID, VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.VerificationVerified, v1.Result)
	assert.Nil(t, v1.FirstDivergentSequence)

	var original string
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT payload_hash FROM audit_events WHERE sequence_number=2`).Scan(&original))
	_, err = admin.Exec(ctx,
		`UPDATE audit_events SET payload_hash='tampered-hash' WHERE sequence_number=2`)
	require.NoError(t, err)

	v2, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: archive.ArchiveID, VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.VerificationDiverged, v2.Result,
		"tampering with an already-archived row's payload_hash must be detected on re-verification")
	require.NotNil(t, v2.FirstDivergentSequence)

	_, err = admin.Exec(ctx,
		`UPDATE audit_events SET payload_hash=$1 WHERE sequence_number=2`, original)
	require.NoError(t, err)
	v3, err := pgStore.VerifyArchive(ctx, domain.VerifyArchiveParams{
		ArchiveID: archive.ArchiveID, VerifiedByPrincipalID: "reviewer-1",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.VerificationVerified, v3.Result, "restoring the row must verify clean again")

	history, err := pgStore.ListVerifications(ctx, archive.ArchiveID)
	require.NoError(t, err)
	assert.Len(t, history, 3, "every verification attempt — VERIFIED or DIVERGED — must be permanently recorded")
}

// TestPgStore_Archive_IsAppendOnly is the real proof (with a genuine
// negative control, not just a positive assertion) of the
// reject_archive_mutation trigger installed by migration 000004.
func TestPgStore_Archive_IsAppendOnly(t *testing.T) {
	ctx := context.Background()
	admin, port := startRLSPostgres(t, ctx, "audit_archive_immutable_test")
	appPool := appRolePool(t, ctx, admin, port, "audit_archive_immutable_test")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()
	pgStore := store.NewPgStore(appPool, log)

	seedEvents(t, ctx, pgStore, 2)
	archive, err := pgStore.CreateArchive(ctx, domain.CreateArchiveParams{
		FromSequence: 1, ToSequence: 2, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	_, err = appPool.Exec(ctx,
		`UPDATE audit_archives SET archive_digest='forged' WHERE archive_id=$1`, archive.ArchiveID)
	require.Error(t, err, "expected the trigger to refuse mutating an archived record")

	_, err = appPool.Exec(ctx, `DELETE FROM audit_archives WHERE archive_id=$1`, archive.ArchiveID)
	require.Error(t, err, "expected archives to never be deletable")

	// Negative control: disable the trigger on the admin connection, confirm
	// the same UPDATE now succeeds (proving the trigger — not something
	// else — was what refused it above), then re-enable and confirm
	// refusal returns.
	_, err = admin.Exec(ctx, `ALTER TABLE audit_archives DISABLE TRIGGER trg_reject_archive_update`)
	require.NoError(t, err)
	_, err = appPool.Exec(ctx,
		`UPDATE audit_archives SET archive_digest='forged-while-disabled' WHERE archive_id=$1`, archive.ArchiveID)
	require.NoError(t, err, "with the trigger disabled the UPDATE must succeed — proving the trigger was the real mechanism")

	_, err = admin.Exec(ctx, `ALTER TABLE audit_archives ENABLE TRIGGER trg_reject_archive_update`)
	require.NoError(t, err)
	_, err = appPool.Exec(ctx,
		`UPDATE audit_archives SET archive_digest='forged-again' WHERE archive_id=$1`, archive.ArchiveID)
	require.Error(t, err, "re-enabling the trigger must restore the refusal")
}
