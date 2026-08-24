package store_test

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

// Row-level security tests for evidence-manifest-svc (migration 000003,
// tracker row 14).
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing about
// the policy.
//
// Note the contrast with migration 000002's append-only trigger, which this
// service already had: a trigger DOES bind a superuser, which is why that
// was the right tool for immutability and is not a substitute for RLS on
// the tenant boundary. Two different mechanisms, two different threats.

const (
	tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"

	_, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`)
	require.NoError(t, err)

	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
	} {
		_, err := admin.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	u.User = url.UserPassword(appRole, appPassword)
	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var isSuper, bypassRLS bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS))
	require.False(t, isSuper, "app role must be NOSUPERUSER — a superuser bypasses RLS entirely")
	require.False(t, bypassRLS, "app role must be NOBYPASSRLS")
	return pool
}

func seedManifestWithRecord(t *testing.T, s *store.PgStore, tenantID string) string {
	t.Helper()
	ctx := tenantCtx(tenantID)
	m := &domain.EvidenceManifest{
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ScenarioType:  domain.ScenarioAudit,
		RequestedBy:   "principal-" + tenantID[:4],
	}
	require.NoError(t, s.CreateManifest(ctx, m))
	require.NoError(t, s.AddRecord(ctx, &domain.ManifestRecord{
		ManifestID:     m.ManifestID,
		SourceType:     domain.SourceGovernanceDecision,
		SourceRecordID: "gd-" + tenantID[:4],
		RecordSnapshot: []byte(`{"confidential":"` + tenantID + `-only"}`),
	}))
	return m.ManifestID
}

func TestRLS_EnabledAndForced(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"evidence_manifests", "manifest_records"} {
		var enabled, forced bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced), table)
		require.True(t, enabled, "%s: migration 000003 must ENABLE row level security", table)
		require.True(t, forced, "%s: migration 000003 must FORCE row level security", table)
	}
}

// TestRLS_ManifestIsolation covers evidence_manifests, which carries
// tenant_id directly.
func TestRLS_ManifestIsolation(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	manifestA := seedManifestWithRecord(t, s, tenantA)
	_ = seedManifestWithRecord(t, s, tenantB)

	_, err := s.FindManifestByID(tenantCtx(tenantB), manifestA)
	require.ErrorIs(t, err, domain.ErrManifestNotFound,
		"tenant B must not read tenant A's manifest")

	own, err := s.FindManifestByID(tenantCtx(tenantA), manifestA)
	require.NoError(t, err, "tenant A must still read its own manifest")
	require.Equal(t, tenantA, own.TenantID)
}

// TestRLS_RecordSnapshotIsolation is the one that matters most.
//
// manifest_records has no tenant column; it reaches the tenant through
// manifest_id. record_snapshot is a verbatim copy of each source record, so
// an unscoped read here returned another tenant's governance decisions,
// access decisions and workflow instances in full — the evidence itself,
// not metadata about it.
func TestRLS_RecordSnapshotIsolation(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	manifestA := seedManifestWithRecord(t, s, tenantA)

	records, err := s.ListRecords(tenantCtx(tenantB), manifestA)
	require.NoError(t, err, "a refused read should be empty, not an error")
	require.Empty(t, records, "tenant B must receive none of tenant A's evidence records")

	own, err := s.ListRecords(tenantCtx(tenantA), manifestA)
	require.NoError(t, err)
	require.Len(t, own, 1, "tenant A must still see its own record")
	require.Contains(t, string(own[0].RecordSnapshot), tenantA,
		"tenant A's own snapshot must come back intact")
}

// TestRLS_TenantlessContext_SeesNothing pins the fail-closed default. The
// handler middleware refuses a request with no X-Tenant-Id, so this path is
// unreachable over HTTP — but the store is callable in-process, and "" must
// match no row rather than every row.
func TestRLS_TenantlessContext_SeesNothing(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	manifestA := seedManifestWithRecord(t, s, tenantA)

	_, err := s.FindManifestByID(context.Background(), manifestA)
	require.ErrorIs(t, err, domain.ErrManifestNotFound,
		"a tenant-less context must read nothing, not everything")

	records, err := s.ListRecords(context.Background(), manifestA)
	require.NoError(t, err)
	require.Empty(t, records, "a tenant-less context must see no evidence records")
}

// TestRLS_FinalizeForeignManifest_Refused covers the write side. Unscoped,
// FinalizeFailed was a denial-of-evidence primitive: any caller holding
// another tenant's manifest id could mark their in-flight bundle FAILED,
// and FAILED is terminal — a retry produces a new manifest rather than
// repairing this one. Doc 03 §22 requires evidence to fail safe; being able
// to fail somebody else's on demand is the inverse.
func TestRLS_FinalizeForeignManifest_Refused(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	manifestA := seedManifestWithRecord(t, s, tenantA)

	_, err := s.FinalizeFailed(tenantCtx(tenantB), manifestA, "forged failure")
	require.ErrorIs(t, err, domain.ErrManifestNotFound,
		"tenant B must not be able to fail tenant A's manifest")

	_, err = s.FinalizeGenerated(tenantCtx(tenantB), manifestA, "forged-checksum")
	require.ErrorIs(t, err, domain.ErrManifestNotFound,
		"tenant B must not be able to write a checksum onto tenant A's manifest")

	// And tenant A's manifest must still be PENDING — the refusals above
	// have to mean "nothing happened", not just "the response said no".
	still, err := s.FindManifestByID(tenantCtx(tenantA), manifestA)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, still.Status,
		"tenant B's refused writes must not have changed tenant A's manifest")
	require.Nil(t, still.ChecksumSHA256)
}

// TestRLS_AddRecordToForeignManifest_Refused covers the derived table's
// WITH CHECK: a record cannot be appended to a manifest the caller cannot
// see. Without it, one tenant could inject records into another tenant's
// evidence bundle — which is worse than reading it, since the bundle is
// what gets handed to a regulator.
func TestRLS_AddRecordToForeignManifest_Refused(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	manifestA := seedManifestWithRecord(t, s, tenantA)

	err := s.AddRecord(tenantCtx(tenantB), &domain.ManifestRecord{
		ManifestID:     manifestA,
		SourceType:     domain.SourceGovernanceDecision,
		SourceRecordID: "injected",
		RecordSnapshot: []byte(`{"injected":true}`),
	})
	require.Error(t, err, "WITH CHECK must refuse a record appended to another tenant's manifest")

	own, err := s.ListRecords(tenantCtx(tenantA), manifestA)
	require.NoError(t, err)
	require.Len(t, own, 1, "tenant A's bundle must still contain only its own record")
	require.NotContains(t, string(own[0].RecordSnapshot), "injected")
}

// TestRLS_ParentPolicyCoupling pins how manifest_records depends on
// evidence_manifests' policy, and records a correction established
// empirically in commercial-account-svc (tracker row 11c).
//
// The intuitive guess — that dropping the parent POLICY widens the child —
// is wrong. Postgres treats RLS-enabled-with-no-applicable-policy as
// deny-all, so the subquery returns the empty set and manifest_records gets
// MORE restrictive: an outage, not a breach. The widening path is DISABLE
// ROW LEVEL SECURITY on the parent. Both are asserted because they look
// equally innocuous in a diff and behave in opposite directions.
func TestRLS_ParentPolicyCoupling(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	ctx := context.Background()

	manifestA := seedManifestWithRecord(t, s, tenantA)

	// Baseline.
	records, err := s.ListRecords(tenantCtx(tenantB), manifestA)
	require.NoError(t, err)
	require.Empty(t, records, "precondition: tenant B already isolated")

	t.Run("dropping the parent policy fails CLOSED", func(t *testing.T) {
		_, err := admin.Exec(ctx, `DROP POLICY tenant_isolation_policy ON evidence_manifests`)
		require.NoError(t, err)
		defer func() {
			_, _ = admin.Exec(ctx, `
				CREATE POLICY tenant_isolation_policy ON evidence_manifests
					FOR ALL
					USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
					WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))`)
		}()

		// Even the OWNING tenant loses its records.
		own, err := s.ListRecords(tenantCtx(tenantA), manifestA)
		require.NoError(t, err)
		require.Empty(t, own, "with the parent policy dropped, deny-all applies even to the owner")
	})

	// Disabling the parent's RLS is the direction that widens the POLICY.
	// Here that does NOT widen the store method, and the difference is worth
	// stating: ListRecords joins evidence_manifests with an explicit
	// m.tenant_id predicate, so the Go query isolates independently of the
	// policy. commercial-account-svc's equivalent read (row 11c) had no
	// explicit predicate — the derived table has no tenant column and the
	// policy was the only boundary — so there, disabling parent RLS did
	// widen it.
	//
	// Both halves are asserted below: the store stays isolated, AND a raw
	// query that relies on the policy alone widens. That second half is what
	// makes the explicit join predicate demonstrably load-bearing rather
	// than decorative.
	t.Run("disabling parent RLS widens the policy but not the scoped query", func(t *testing.T) {
		_, err := admin.Exec(ctx, `ALTER TABLE evidence_manifests DISABLE ROW LEVEL SECURITY`)
		require.NoError(t, err)
		defer func() {
			_, _ = admin.Exec(ctx, `ALTER TABLE evidence_manifests ENABLE ROW LEVEL SECURITY`)
			_, _ = admin.Exec(ctx, `ALTER TABLE evidence_manifests FORCE ROW LEVEL SECURITY`)
		}()

		// (a) The store method still isolates — its own predicate holds.
		stillIsolated, err := s.ListRecords(tenantCtx(tenantB), manifestA)
		require.NoError(t, err)
		require.Empty(t, stillIsolated,
			"ListRecords joins on evidence_manifests.tenant_id, so it must isolate even with the policy weakened")

		// (b) A query relying on the policy alone DOES widen. This is what
		// the explicit predicate is protecting against, and what row 11c
		// observed in a service that lacked one.
		appPool := appRolePool(t, admin)
		conn, err := appPool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()

		_, err = conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantB)
		require.NoError(t, err)

		var visible int
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT count(*) FROM manifest_records WHERE manifest_id = $1`, manifestA,
		).Scan(&visible))
		require.Equal(t, 1, visible,
			"with parent RLS disabled, the policy alone no longer isolates manifest_records — "+
				"one ALTER TABLE on evidence_manifests exposes every tenant's evidence snapshots "+
				"to any query that does not carry its own tenant predicate")
		t.Logf("confirmed coupling: policy-only visibility of tenant A's records from tenant B = %d row(s); "+
			"the store's explicit join is what still refused it", visible)
	})

	// Restored state must isolate again, so a failure above cannot leave a
	// later test passing against a weakened schema.
	after, err := s.ListRecords(tenantCtx(tenantB), manifestA)
	require.NoError(t, err)
	require.Empty(t, after, "cleanup failed: tenant B still sees tenant A's records")
}
