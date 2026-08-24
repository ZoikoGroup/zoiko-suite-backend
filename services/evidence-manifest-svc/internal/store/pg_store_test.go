package store_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	// Apply EVERY migration in filename order, never one hardcoded name.
	// This previously read 000001 alone, which meant migration 000002's
	// append-only trigger was never exercised and 000003's RLS policies
	// would have been skipped silently — the tests would have gone green
	// while the boundary under test was absent. Same failure mode found in
	// audit-event-store-svc and configuration-feature-flag-svc during
	// Priority 1.
	entries, err := os.ReadDir("../../deployments/migrations")
	require.NoError(t, err)
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			migs = append(migs, e.Name())
		}
	}
	require.NotEmpty(t, migs, "no migrations found")
	sort.Strings(migs)
	for _, name := range migs {
		sql, err := os.ReadFile(filepath.Join("../../deployments/migrations", name))
		require.NoError(t, err, name)
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoError(t, err, name)
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
