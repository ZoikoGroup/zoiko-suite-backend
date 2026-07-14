package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
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

	sql, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(sql))
	require.NoError(t, err)

	return pool
}

func TestPgStore_CreateManifest_And_FindByID(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	m := &domain.EvidenceManifest{
		TenantID: "11111111-1111-1111-1111-111111111111", LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ScenarioType: domain.ScenarioAudit, RequestedBy: "principal-1",
	}
	require.NoError(t, s.CreateManifest(context.Background(), m))
	require.NotEmpty(t, m.ManifestID)
	require.Equal(t, domain.StatusPending, m.Status)

	found, err := s.FindManifestByID(context.Background(), m.ManifestID)
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
	require.NoError(t, s.CreateManifest(context.Background(), m))

	for i := 0; i < 3; i++ {
		require.NoError(t, s.AddRecord(context.Background(), &domain.ManifestRecord{
			ManifestID: m.ManifestID, SourceType: domain.SourceGovernanceDecision,
			SourceRecordID: "gd-" + string(rune('1'+i)), RecordSnapshot: []byte(`{"x":1}`),
		}))
	}

	records, err := s.ListRecords(context.Background(), m.ManifestID)
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
	require.NoError(t, s.CreateManifest(context.Background(), m))

	finalized, err := s.FinalizeGenerated(context.Background(), m.ManifestID, "abc123checksum")
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
	require.NoError(t, s.CreateManifest(context.Background(), m))

	failed, err := s.FinalizeFailed(context.Background(), m.ManifestID, "authorization-svc unreachable")
	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, failed.Status)
	require.NotNil(t, failed.FailureReason)
	require.Equal(t, "authorization-svc unreachable", *failed.FailureReason)
}

func TestPgStore_FindManifestByID_UnknownID_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.FindManifestByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	require.ErrorIs(t, err, domain.ErrManifestNotFound)
}
