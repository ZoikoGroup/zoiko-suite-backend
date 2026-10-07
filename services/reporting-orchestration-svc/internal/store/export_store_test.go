package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// requireTestDB connects to a real Postgres named by TEST_DATABASE_URL and
// replays every migration from a clean slate — same pattern used
// throughout this build for services with no prior store test file.
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
		DROP TABLE IF EXISTS delivery_receipts;
		DROP TABLE IF EXISTS redaction_decisions;
		DROP TABLE IF EXISTS export_manifests;
		DROP TABLE IF EXISTS audit_export_requests;
		DROP TABLE IF EXISTS report_runs;
		DROP TABLE IF EXISTS report_definitions;
	`)
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no migrations found in %s", migDir)
	sort.Strings(migrations)
	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		require.NoErrorf(t, err, "reading migration %s", filepath.Base(path))
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoErrorf(t, err, "applying migration %s", filepath.Base(path))
	}
	return pool
}

func ctx() context.Context { return context.Background() }

func newExportForTest(t *testing.T, s *store.PgStore, tenantID, requestedBy string) *domain.AuditExportRequest {
	t.Helper()
	e, err := s.CreateExportRequest(ctx(), domain.CreateExportRequestParams{
		TenantID: tenantID, LegalEntityID: "le-1", ArchiveID: "archive-fixture-1",
		Purpose: "regulator request", RequestedByPrincipalID: requestedBy,
	})
	require.NoError(t, err)
	return e
}

// TestPgStore_ApproveExport_RefusesSelfApproval is the real, DB-enforced
// proof of AUD-10's approval-segregation control.
func TestPgStore_ApproveExport_RefusesSelfApproval(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	e := newExportForTest(t, s, "tenant-a", "auditor-1")

	_, err := s.ApproveExport(ctx(), domain.ApproveExportParams{
		ExportID: e.ExportID, TenantID: "tenant-a", ApprovedByPrincipalID: "auditor-1",
	})
	require.ErrorIs(t, err, domain.ErrExportSelfApproval)

	approved, err := s.ApproveExport(ctx(), domain.ApproveExportParams{
		ExportID: e.ExportID, TenantID: "tenant-a", ApprovedByPrincipalID: "partner-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.ExportApproved, approved.Status)
}

// TestPgStore_ApproveExport_SelfApprovalCheckConstraint is the negative
// control for the OTHER half of the defense-in-depth: migration 000002's
// own audit_export_requests_no_self_approval CHECK constraint, independent
// of the store's CAS predicate tested above. Dropping it and re-attempting
// the exact self-approval this CHECK exists to block proves the CHECK —
// not just the application-layer CAS clause — is a real, separate line of
// defense.
func TestPgStore_ApproveExport_SelfApprovalCheckConstraint(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	e := newExportForTest(t, s, "tenant-a", "auditor-1")

	_, err := pool.Exec(ctx(), `UPDATE audit_export_requests SET approved_by_principal_id='auditor-1' WHERE export_id=$1`, e.ExportID)
	require.Error(t, err, "expected the CHECK constraint to refuse a raw self-approval UPDATE, independent of the store's own CAS predicate")

	_, err = pool.Exec(ctx(), `ALTER TABLE audit_export_requests DROP CONSTRAINT audit_export_requests_no_self_approval`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(), `UPDATE audit_export_requests SET approved_by_principal_id='auditor-1' WHERE export_id=$1`, e.ExportID)
	require.NoError(t, err, "with the CHECK dropped the same UPDATE must succeed — proving the CHECK was the real mechanism above")

	_, err = pool.Exec(ctx(), `ALTER TABLE audit_export_requests ADD CONSTRAINT audit_export_requests_no_self_approval
		CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> requested_by_principal_id) NOT VALID`)
	require.NoError(t, err)
}

// TestPgStore_SealExport_RequiresManifest is the real proof that an export
// cannot be sealed empty: SealExport's CAS predicate only succeeds once at
// least one manifest row exists.
func TestPgStore_SealExport_RequiresManifest(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	e := newExportForTest(t, s, "tenant-a", "auditor-1")
	_, err := s.ApproveExport(ctx(), domain.ApproveExportParams{ExportID: e.ExportID, TenantID: "tenant-a", ApprovedByPrincipalID: "partner-1"})
	require.NoError(t, err)
	require.NoError(t, s.MarkExportBuilding(ctx(), "tenant-a", e.ExportID))

	_, err = s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: "tenant-a"})
	require.ErrorIs(t, err, domain.ErrManifestRequired)

	_, err = s.RecordManifestEntry(ctx(), "tenant-a", e.ExportID, "audit-archive-fixture-1", "deadbeef")
	require.NoError(t, err)

	sealed, err := s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: "tenant-a"})
	require.NoError(t, err)
	require.Equal(t, domain.ExportSealed, sealed.Status)
}

// TestPgStore_DeliverExport_HappyPathAndRevoke exercises the full
// approve→build→manifest→seal→deliver→revoke lifecycle end to end.
func TestPgStore_DeliverExport_HappyPathAndRevoke(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	e := newExportForTest(t, s, "tenant-a", "auditor-1")
	_, err := s.ApproveExport(ctx(), domain.ApproveExportParams{ExportID: e.ExportID, TenantID: "tenant-a", ApprovedByPrincipalID: "partner-1"})
	require.NoError(t, err)
	require.NoError(t, s.MarkExportBuilding(ctx(), "tenant-a", e.ExportID))
	_, err = s.RecordManifestEntry(ctx(), "tenant-a", e.ExportID, "audit-archive-fixture-1", "deadbeef")
	require.NoError(t, err)
	_, err = s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: "tenant-a"})
	require.NoError(t, err)

	delivered, err := s.DeliverExport(ctx(), domain.DeliverExportParams{
		ExportID: e.ExportID, TenantID: "tenant-a", DeliveredToPrincipalID: "regulator-1", DeliveryChannel: "secure-portal",
	})
	require.NoError(t, err)
	require.Equal(t, domain.ExportDelivered, delivered.Status)

	revoked, err := s.RevokeExport(ctx(), domain.RevokeExportParams{ExportID: e.ExportID, TenantID: "tenant-a"})
	require.NoError(t, err)
	require.Equal(t, domain.ExportRevoked, revoked.Status)
}

// TestPgStore_ExportChildTables_AreAppendOnly is the real proof (with a
// genuine negative control) of the reject_export_child_mutation trigger
// installed by migration 000002.
func TestPgStore_ExportChildTables_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	e := newExportForTest(t, s, "tenant-a", "auditor-1")
	m, err := s.RecordManifestEntry(ctx(), "tenant-a", e.ExportID, "audit-archive-fixture-1", "deadbeef")
	require.NoError(t, err)

	_, err = pool.Exec(ctx(), `UPDATE export_manifests SET sha256='forged' WHERE manifest_id=$1`, m.ManifestID)
	require.Error(t, err, "expected the trigger to refuse mutating a manifest entry")
	_, err = pool.Exec(ctx(), `DELETE FROM export_manifests WHERE manifest_id=$1`, m.ManifestID)
	require.Error(t, err, "expected manifest entries to never be deletable")

	// Negative control: disable the trigger, confirm the same UPDATE now
	// succeeds (proving the trigger — not something else — was refusing
	// it), then re-enable and confirm refusal returns.
	_, err = pool.Exec(ctx(), `ALTER TABLE export_manifests DISABLE TRIGGER trg_reject_export_manifests_update`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(), `UPDATE export_manifests SET sha256='forged-while-disabled' WHERE manifest_id=$1`, m.ManifestID)
	require.NoError(t, err, "with the trigger disabled the UPDATE must succeed — proving the trigger was the real mechanism")

	_, err = pool.Exec(ctx(), `ALTER TABLE export_manifests ENABLE TRIGGER trg_reject_export_manifests_update`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(), `UPDATE export_manifests SET sha256='forged-again' WHERE manifest_id=$1`, m.ManifestID)
	require.Error(t, err, "re-enabling the trigger must restore the refusal")
}
