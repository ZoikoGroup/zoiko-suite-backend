package store_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS manifest_records;
		DROP TABLE IF EXISTS evidence_manifests;
	`)
	require.NoError(t, err)

	// EVERY *.up.sql, BY GLOB, NOT ONE NAMED FILE. This applied 000001 alone and
	// the service has two, so 000002_enforce_immutability was never present —
	// the append-only triggers on an evidence manifest were absent from the
	// schema these tests ran against, which is the one property a manifest has.
	migrations, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	require.NoError(t, err, "globbing migrations")
	require.NotEmpty(t, migrations, "no *.up.sql found — these tests would run against an empty schema")
	sort.Strings(migrations)

	for _, path := range migrations {
		sql, readErr := os.ReadFile(path)
		require.NoError(t, readErr, "reading %s", filepath.Base(path))
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoError(t, err, "applying %s", filepath.Base(path))
	}

	return pool
}

// tenantCtx is the context these tests use for store calls.
//
// RLS is now in force, so a store call with no tenant on the context sees
// and writes nothing — which is the intended fail-closed behaviour, and the
// reason these tests must state a tenant explicitly rather than using
// context.Background(). Before migration 000003 they passed with no tenant
// at all, because nothing in the store looked for one.
func tenantCtx(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

const testTenant = "11111111-1111-1111-1111-111111111111"

func TestPgStore_CreateManifest_And_FindByID(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	m := &domain.EvidenceManifest{
		TenantID: "11111111-1111-1111-1111-111111111111", LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ScenarioType: domain.ScenarioAudit, RequestedBy: "principal-1",
	}
	require.NoError(t, s.CreateManifest(tenantCtx(testTenant), m))
	require.NotEmpty(t, m.ManifestID)
	require.Equal(t, domain.StatusPending, m.Status)

	found, err := s.FindManifestByID(tenantCtx(testTenant), m.ManifestID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, found.Status)
}

func TestPgStore_AddRecord_And_ListRecords(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	m := &domain.EvidenceManifest{
		TenantID: "11111111-1111-1111-1111-111111111111", LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ScenarioType: domain.ScenarioRegulator, RequestedBy: "principal-1",
	}
	require.NoError(t, s.CreateManifest(tenantCtx(testTenant), m))

	for i := 0; i < 3; i++ {
		require.NoError(t, s.AddRecord(tenantCtx(testTenant), &domain.ManifestRecord{
			ManifestID: m.ManifestID, SourceType: domain.SourceGovernanceDecision,
			SourceRecordID: "gd-" + string(rune('1'+i)), RecordSnapshot: []byte(`{"x":1}`),
		}))
	}

	records, err := s.ListRecords(tenantCtx(testTenant), m.ManifestID)
	require.NoError(t, err)
	require.Len(t, records, 3, "every added record must remain — append-only")
}

func TestPgStore_FinalizeGenerated_SetsChecksumAndStatus(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	m := &domain.EvidenceManifest{
		TenantID: "11111111-1111-1111-1111-111111111111", LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ScenarioType: domain.ScenarioComplianceReview, RequestedBy: "principal-1",
	}
	require.NoError(t, s.CreateManifest(tenantCtx(testTenant), m))

	finalized, err := s.FinalizeGenerated(tenantCtx(testTenant), m.ManifestID, "abc123checksum")
	require.NoError(t, err)
	require.Equal(t, domain.StatusGenerated, finalized.Status)
	require.NotNil(t, finalized.ChecksumSHA256)
	require.Equal(t, "abc123checksum", *finalized.ChecksumSHA256)
	require.NotNil(t, finalized.GeneratedAt)
}

func TestPgStore_FinalizeFailed_SetsReasonAndStatus(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	m := &domain.EvidenceManifest{
		TenantID: "11111111-1111-1111-1111-111111111111", LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ScenarioType: domain.ScenarioLegalDiscovery, RequestedBy: "principal-1",
	}
	require.NoError(t, s.CreateManifest(tenantCtx(testTenant), m))

	failed, err := s.FinalizeFailed(tenantCtx(testTenant), m.ManifestID, "authorization-svc unreachable")
	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, failed.Status)
	require.NotNil(t, failed.FailureReason)
	require.Equal(t, "authorization-svc unreachable", *failed.FailureReason)
}

func TestPgStore_FindManifestByID_UnknownID_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.FindManifestByID(tenantCtx(testTenant), "00000000-0000-0000-0000-000000000000")
	require.ErrorIs(t, err, domain.ErrManifestNotFound)
}
