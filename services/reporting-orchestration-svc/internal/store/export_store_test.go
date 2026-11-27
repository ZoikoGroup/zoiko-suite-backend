package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// requireTestDB connects to the Postgres named by TEST_DATABASE_URL, which
// must already have every up migration applied. Run it as a NOSUPERUSER
// NOBYPASSRLS role that owns the tables: FORCE ROW LEVEL SECURITY then
// applies to it, and as owner it can run the DDL negative controls below.
//
// (The pre-rewrite version dropped and replayed the schema here; the tables
// are now shared with the handler integration tests, which run in a
// parallel package, so the schema is applied once up front instead.)
func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(context.Background()))
	return pool
}

func ctx() context.Context { return context.Background() }

func newTenant() string { return "tenant-" + uuid.NewString() }

// inTenant runs raw SQL inside a transaction scoped to tenantID, the way
// the store does, so RLS admits the tenant's own rows.
func inTenant(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := pool.Begin(ctx())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx()) }()
	_, err = tx.Exec(ctx(), "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx())
}

func newExportForTest(t *testing.T, s *store.PgStore, tenantID, requestedBy string) *domain.AuditExportRequest {
	t.Helper()
	e, err := s.CreateExportRequest(ctx(), domain.CreateExportRequestParams{
		TenantID: tenantID, LegalEntityID: "le-1", ArchiveID: "archive-fixture-1",
		Purpose: "regulator request", RequestedByPrincipalID: requestedBy,
	})
	require.NoError(t, err)
	return e
}

const fixtureDigest = "a3f5b1c2d4e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f80"

// buildToValidating ports the removed MarkExportBuilding+RecordManifestEntry
// pair onto the rewrite's StartBuild/CompleteBuild.
func buildToValidating(t *testing.T, s *store.PgStore, tenantID, exportID string) {
	t.Helper()
	_, err := s.StartBuild(ctx(), domain.StartBuildParams{ExportID: exportID, TenantID: tenantID})
	require.NoError(t, err)
	_, err = s.CompleteBuild(ctx(), domain.CompleteBuildParams{
		ExportID: exportID, TenantID: tenantID, ArtifactName: "audit-archive-fixture-1", Sha256: fixtureDigest,
	})
	require.NoError(t, err)
}

func approve(t *testing.T, s *store.PgStore, tenantID, exportID string) {
	t.Helper()
	_, err := s.ApproveExport(ctx(), domain.ApproveExportParams{ExportID: exportID, TenantID: tenantID, ApprovedByPrincipalID: "partner-1"})
	require.NoError(t, err)
}

// TestPgStore_ApproveExport_RefusesSelfApproval is the real, DB-enforced
// proof of AUD-10's approval-segregation control.
func TestPgStore_ApproveExport_RefusesSelfApproval(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")

	_, err := s.ApproveExport(ctx(), domain.ApproveExportParams{
		ExportID: e.ExportID, TenantID: tenant, ApprovedByPrincipalID: "auditor-1",
	})
	require.ErrorIs(t, err, domain.ErrExportSelfApproval)

	approved, err := s.ApproveExport(ctx(), domain.ApproveExportParams{
		ExportID: e.ExportID, TenantID: tenant, ApprovedByPrincipalID: "partner-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.ExportApproved, approved.Status)
}

// TestPgStore_ApproveExport_SelfApprovalCheckConstraint is the negative
// control for migration 000002's audit_export_requests_no_self_approval
// CHECK constraint, independent of the store's CAS predicate.
func TestPgStore_ApproveExport_SelfApprovalCheckConstraint(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")
	selfApprove := func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx(), `UPDATE audit_export_requests SET approved_by_principal_id='auditor-1' WHERE export_id=$1`, e.ExportID)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("update matched no row (RLS scope wrong)")
		}
		return err
	}

	require.Error(t, inTenant(t, pool, tenant, selfApprove),
		"expected the CHECK constraint to refuse a raw self-approval UPDATE, independent of the store's own CAS predicate")

	// Negative control inside one rolled-back transaction, so the
	// constraint is never actually missing for concurrently running tests.
	tx, err := pool.Begin(ctx())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx()) }()
	_, err = tx.Exec(ctx(), "SELECT set_config('app.tenant_id', $1, true)", tenant)
	require.NoError(t, err)
	_, err = tx.Exec(ctx(), `ALTER TABLE audit_export_requests DROP CONSTRAINT audit_export_requests_no_self_approval`)
	require.NoError(t, err)
	require.NoError(t, selfApprove(tx), "with the CHECK dropped the same UPDATE must succeed — proving the CHECK was the real mechanism above")
}

// TestPgStore_SealExport_RequiresManifest: an export cannot be sealed empty.
func TestPgStore_SealExport_RequiresManifest(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")
	approve(t, s, tenant, e.ExportID)
	_, err := s.StartBuild(ctx(), domain.StartBuildParams{ExportID: e.ExportID, TenantID: tenant})
	require.NoError(t, err)

	// BUILDING is not sealable; force VALIDATING with no manifest row to
	// prove the empty-package guard itself.
	_, err = s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: tenant, SealedByPrincipalID: "auditor-1"})
	require.ErrorIs(t, err, domain.ErrExportInvalidState)
	require.NoError(t, inTenant(t, pool, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx(), `UPDATE audit_export_requests SET status='VALIDATING' WHERE export_id=$1`, e.ExportID)
		return err
	}))
	_, err = s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: tenant, SealedByPrincipalID: "auditor-1"})
	require.ErrorIs(t, err, domain.ErrManifestRequired)

	// A properly built export seals.
	e2 := newExportForTest(t, s, tenant, "auditor-1")
	approve(t, s, tenant, e2.ExportID)
	buildToValidating(t, s, tenant, e2.ExportID)
	sealed, err := s.SealExport(ctx(), domain.SealExportParams{ExportID: e2.ExportID, TenantID: tenant, SealedByPrincipalID: "auditor-1"})
	require.NoError(t, err)
	require.Equal(t, domain.ExportSealed, sealed.Status)
	require.Len(t, sealed.SealDigest, 64)
}

// TestPgStore_DeliverExport_HappyPathAndRevoke exercises the full
// approve→build→manifest→seal→deliver→revoke lifecycle end to end.
func TestPgStore_DeliverExport_HappyPathAndRevoke(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")
	approve(t, s, tenant, e.ExportID)
	buildToValidating(t, s, tenant, e.ExportID)
	_, err := s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: tenant, SealedByPrincipalID: "auditor-1"})
	require.NoError(t, err)

	delivered, err := s.DeliverExport(ctx(), domain.DeliverExportParams{
		ExportID: e.ExportID, TenantID: tenant, DeliveredToPrincipalID: "regulator-1", DeliveryChannel: "secure-portal",
	})
	require.NoError(t, err)
	require.Equal(t, domain.ExportDelivered, delivered.Status)

	// Migration 000003 requires who and why on a revocation.
	revoked, err := s.RevokeExport(ctx(), domain.RevokeExportParams{
		ExportID: e.ExportID, TenantID: tenant, RevokedByPrincipalID: "auditor-1", Reason: "engagement ended",
	})
	require.NoError(t, err)
	require.Equal(t, domain.ExportRevoked, revoked.Status)
}

// TestPgStore_BuildFailureIsTerminal: an archive failure moves the export to
// FAILED with its reason; a failed export can never be built or sealed.
func TestPgStore_BuildFailureIsTerminal(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")
	approve(t, s, tenant, e.ExportID)
	_, err := s.StartBuild(ctx(), domain.StartBuildParams{ExportID: e.ExportID, TenantID: tenant})
	require.NoError(t, err)
	failed, err := s.FailBuild(ctx(), domain.FailBuildParams{ExportID: e.ExportID, TenantID: tenant, Reason: "archive fetch failed: 404"})
	require.NoError(t, err)
	require.Equal(t, domain.ExportFailed, failed.Status)
	require.Equal(t, "archive fetch failed: 404", failed.FailureReason)

	_, err = s.StartBuild(ctx(), domain.StartBuildParams{ExportID: e.ExportID, TenantID: tenant})
	require.ErrorIs(t, err, domain.ErrExportInvalidState)
	_, err = s.SealExport(ctx(), domain.SealExportParams{ExportID: e.ExportID, TenantID: tenant})
	require.ErrorIs(t, err, domain.ErrExportInvalidState)
	_, entries, _, err := s.GetPackageManifest(ctx(), tenant, e.ExportID)
	require.NoError(t, err)
	require.Empty(t, entries, "a failed build must not leave a manifest behind")
}

// TestPgStore_StaleBuildIsReclaimed: a build whose claim committed but whose
// outcome never did (process died mid-build) stays BUILDING. Within the
// lease a second build is refused (no duplicate work); once the lease has
// expired a retried build re-claims it and completes exactly once.
func TestPgStore_StaleBuildIsReclaimed(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")
	approve(t, s, tenant, e.ExportID)
	first, err := s.StartBuild(ctx(), domain.StartBuildParams{ExportID: e.ExportID, TenantID: tenant})
	require.NoError(t, err)
	require.Equal(t, 1, first.BuildAttempts)

	_, err = s.StartBuild(ctx(), domain.StartBuildParams{ExportID: e.ExportID, TenantID: tenant})
	require.ErrorIs(t, err, domain.ErrBuildInProgress, "a live build must not be taken over")

	require.NoError(t, inTenant(t, pool, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx(), `UPDATE audit_export_requests SET build_started_at = now() - interval '1 hour' WHERE export_id=$1`, e.ExportID)
		return err
	}))
	second, err := s.StartBuild(ctx(), domain.StartBuildParams{ExportID: e.ExportID, TenantID: tenant})
	require.NoError(t, err)
	require.Equal(t, domain.ExportBuilding, second.Status)
	require.Equal(t, 2, second.BuildAttempts)

	_, err = s.CompleteBuild(ctx(), domain.CompleteBuildParams{ExportID: e.ExportID, TenantID: tenant, ArtifactName: "a", Sha256: fixtureDigest})
	require.NoError(t, err)
	// The original (presumed dead) worker finishing late changes nothing.
	_, err = s.CompleteBuild(ctx(), domain.CompleteBuildParams{ExportID: e.ExportID, TenantID: tenant, ArtifactName: "a", Sha256: fixtureDigest})
	require.ErrorIs(t, err, domain.ErrExportInvalidState)
	_, entries, _, err := s.GetPackageManifest(ctx(), tenant, e.ExportID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// TestPgStore_ExportChildTables_AreAppendOnly is the real proof (with a
// genuine negative control) of the reject_export_child_mutation trigger
// installed by migration 000002.
func TestPgStore_ExportChildTables_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenant := newTenant()

	e := newExportForTest(t, s, tenant, "auditor-1")
	approve(t, s, tenant, e.ExportID)
	buildToValidating(t, s, tenant, e.ExportID)
	_, entries, _, err := s.GetPackageManifest(ctx(), tenant, e.ExportID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	manifestID := entries[0].ManifestID

	forge := func(value string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx(), `UPDATE export_manifests SET sha256=$2 WHERE manifest_id=$1`, manifestID, value)
			if err == nil && tag.RowsAffected() != 1 {
				return errors.New("update matched no row (RLS scope wrong)")
			}
			return err
		}
	}
	require.Error(t, inTenant(t, pool, tenant, forge("forged")), "expected the trigger to refuse mutating a manifest entry")
	require.Error(t, inTenant(t, pool, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx(), `DELETE FROM export_manifests WHERE manifest_id=$1`, manifestID)
		return err
	}), "expected manifest entries to never be deletable")

	// Negative control in a rolled-back transaction: with the trigger
	// disabled the same UPDATE succeeds, proving the trigger was refusing.
	tx, err := pool.Begin(ctx())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx()) }()
	_, err = tx.Exec(ctx(), "SELECT set_config('app.tenant_id', $1, true)", tenant)
	require.NoError(t, err)
	_, err = tx.Exec(ctx(), `ALTER TABLE export_manifests DISABLE TRIGGER trg_reject_export_manifests_update`)
	require.NoError(t, err)
	require.NoError(t, forge("forged-while-disabled")(tx),
		"with the trigger disabled the UPDATE must succeed — proving the trigger was the real mechanism")
}

// requireRLSRole: a superuser or BYPASSRLS role sees every row regardless of
// policy, so an isolation test run as one would pass vacuously.
func requireRLSRole(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super, bypass bool
	require.NoError(t, pool.QueryRow(ctx(),
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass))
	if super || bypass {
		t.Skip("TEST_DATABASE_URL role is SUPERUSER or BYPASSRLS; RLS isolation cannot be proven with it")
	}
}

// TestRLS_TenantIsolation proves isolation is enforced by Postgres itself:
// raw SQL with no store-side scoping sees only the scoped tenant's rows, an
// unscoped transaction sees none, and WITH CHECK refuses a cross-tenant
// insert.
func TestRLS_TenantIsolation(t *testing.T) {
	pool := requireTestDB(t)
	requireRLSRole(t, pool)
	s := store.NewPgStore(pool)
	owner, other := newTenant(), newTenant()

	e, err := s.CreateExportRequest(ctx(), domain.CreateExportRequestParams{
		TenantID: owner, LegalEntityID: "le-1", ArchiveID: "a-1", Purpose: "p", RequestedByPrincipalID: "auditor-1",
		Event: domain.OutboxEvent{EventType: domain.EventExportRequested, AggregateType: domain.AggregateAuditExport, TenantID: owner},
	})
	require.NoError(t, err)
	approve(t, s, owner, e.ExportID)
	buildToValidating(t, s, owner, e.ExportID)

	_, err = s.GetExportRequest(ctx(), other, e.ExportID)
	require.ErrorIs(t, err, domain.ErrExportNotFound)

	tables := []string{"audit_export_requests", "export_manifests", "outbox_events", "report_definitions", "report_runs",
		"redaction_decisions", "delivery_receipts", "idempotency_records"}
	for _, scope := range []string{other, ""} {
		require.NoError(t, inTenant(t, pool, scope, func(tx pgx.Tx) error {
			for _, table := range tables {
				var n int
				if err := tx.QueryRow(ctx(), "SELECT COUNT(*) FROM "+table+" WHERE tenant_id = $1", owner).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					t.Errorf("scope %q saw %d of the owner's rows in %s", scope, n, table)
				}
			}
			return nil
		}))
	}
	// Positive control: the owner's own scope does see them.
	require.NoError(t, inTenant(t, pool, owner, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx(), "SELECT COUNT(*) FROM export_manifests WHERE tenant_id = $1", owner).Scan(&n); err != nil {
			return err
		}
		require.Equal(t, 1, n)
		return nil
	}))

	err = inTenant(t, pool, other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx(), `INSERT INTO audit_export_requests (tenant_id, legal_entity_id, archive_id, purpose, requested_by_principal_id)
			VALUES ($1, 'le', 'a', 'p', 'x')`, owner)
		return err
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "row-level security"), "expected an RLS WITH CHECK violation, got %v", err)
}

// TestOutboxRelay_DeliversAcrossTenantsAndRetries: under FORCE RLS the relay
// (app.outbox_relay) still sees every tenant's events; a failed publish
// leaves the event pending, and a later pass delivers it.
func TestOutboxRelay_DeliversAcrossTenantsAndRetries(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	tenants := []string{newTenant(), newTenant()}
	ids := map[string]bool{}
	for _, tenant := range tenants {
		e, err := s.CreateExportRequest(ctx(), domain.CreateExportRequestParams{
			TenantID: tenant, LegalEntityID: "le-1", ArchiveID: "a-1", Purpose: "p", RequestedByPrincipalID: "x",
			Event: domain.OutboxEvent{EventType: domain.EventExportRequested, AggregateType: domain.AggregateAuditExport, TenantID: tenant},
		})
		require.NoError(t, err)
		ids[e.ExportID] = true
	}

	pending := func(tenant string) int {
		var n int
		require.NoError(t, inTenant(t, pool, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx(), `SELECT COUNT(*) FROM outbox_events WHERE tenant_id=$1 AND published_at IS NULL`, tenant).Scan(&n)
		}))
		return n
	}

	_, err := s.PublishOutboxBatch(ctx(), 10000, func(context.Context, store.PendingOutboxEvent) error {
		return errors.New("broker down")
	})
	require.NoError(t, err)
	for _, tenant := range tenants {
		require.Equal(t, 1, pending(tenant), "failed publish must leave the event pending")
	}

	seen := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for (pending(tenants[0]) > 0 || pending(tenants[1]) > 0) && time.Now().Before(deadline) {
		_, err := s.PublishOutboxBatch(ctx(), 10000, func(_ context.Context, ev store.PendingOutboxEvent) error {
			seen[ev.AggregateID] = true
			return nil
		})
		require.NoError(t, err)
	}
	for id := range ids {
		require.True(t, seen[id], "relay must deliver the event for export %s", id)
	}
}
