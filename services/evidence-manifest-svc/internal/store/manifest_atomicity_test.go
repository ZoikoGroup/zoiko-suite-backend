package store_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

func newManifestInput() *domain.EvidenceManifest {
	return &domain.EvidenceManifest{LegalEntityID: uuid.New().String(), ScenarioType: domain.ScenarioAudit, RequestedBy: "principal-1"}
}

func recs(ids ...string) []domain.ManifestRecord {
	var out []domain.ManifestRecord
	for _, id := range ids {
		out = append(out, domain.ManifestRecord{
			SourceType: domain.SourceGovernanceDecision, SourceRecordID: id,
			RecordSnapshot: []byte(fmt.Sprintf(`{"decision_id":%q,"outcome":"APPROVED"}`, id)),
		})
	}
	return out
}

// footprint counts everything a generation attempt may leave behind for a tenant.
type footprint struct{ manifests, records, outbox int }

func footprintOf(t *testing.T, pool *pgxpool.Pool, tenantID string) footprint {
	t.Helper()
	var f footprint
	ctx := context.Background()
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM evidence_manifests WHERE tenant_id=$1`, tenantID).Scan(&f.manifests))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM manifest_records r JOIN evidence_manifests m USING (manifest_id) WHERE m.tenant_id=$1`, tenantID).Scan(&f.records))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id=$1`, tenantID).Scan(&f.outbox))
	return f
}

// failWhen installs a trigger that raises on the statement under test and
// removes it afterwards. It is how each failure point is reached for real,
// inside the same transaction, without a seam in production code.
func failWhen(t *testing.T, pool *pgxpool.Pool, name, table, when, cond string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %[1]s_fn() RETURNS trigger AS $$
		BEGIN
			IF %[4]s THEN RAISE EXCEPTION 'injected failure: %[1]s'; END IF;
			RETURN NEW;
		END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER %[1]s %[3]s ON %[2]s FOR EACH ROW EXECUTE FUNCTION %[1]s_fn();
	`, name, table, when, cond))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s; DROP FUNCTION IF EXISTS %s_fn();`, name, table, name))
	})
}

func TestCompleteManifest_HappyPath_OneGeneratedManifest_OneEvent_ChecksumMatchesStoredRecords(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)

	in := newManifestInput()
	// Deliberately non-canonical JSON: JSONB re-orders keys and strips
	// whitespace, so a checksum over the SOURCE bytes could never verify.
	rs := recs("gd-1", "gd-2", "gd-3")
	rs[0].RecordSnapshot = []byte(`{"outcome":  "APPROVED",   "decision_id":"gd-1"}`)

	got, err := s.CompleteManifest(ctx, in, rs, "corr-happy")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusGenerated, got.Status)
	require.NotNil(t, got.ChecksumSHA256)
	assert.Equal(t, got.ManifestID, in.ManifestID)

	stored, err := s.ListRecords(ctx, got.ManifestID)
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, domain.ManifestChecksum(stored), *got.ChecksumSHA256,
		"the recorded checksum must be exactly what verification recomputes from the stored records")

	f := footprintOf(t, pool, tenant)
	assert.Equal(t, footprint{manifests: 1, records: 3, outbox: 1}, f)

	var evType, corr string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT event_type, correlation_id FROM outbox_events WHERE aggregate_id=$1`, got.ManifestID).Scan(&evType, &corr))
	assert.Equal(t, "evidence.manifest.generated", evType)
	assert.Equal(t, "corr-happy", corr)
}

// A failure at ANY point must leave no manifest of any status, no records and
// no event. 5 records: first, a middle one and the last are each made to fail.
func TestCompleteManifest_FailureAtAnyRecord_LeavesNothing(t *testing.T) {
	for _, tc := range []struct{ name, boom string }{
		{"first record", "gd-1"}, {"middle record", "gd-3"}, {"last record", "gd-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := requireTestDB(t)
			s := store.New(pool, zap.NewNop())
			tenant := uuid.New().String()
			ctx := svcmiddleware.WithTenant(context.Background(), tenant)
			failWhen(t, pool, "inj_record", "manifest_records", "BEFORE INSERT", fmt.Sprintf("NEW.source_record_id = '%s'", tc.boom))

			_, err := s.CompleteManifest(ctx, newManifestInput(), recs("gd-1", "gd-2", "gd-3", "gd-4", "gd-5"), "corr-x")
			require.Error(t, err)
			assert.Equal(t, footprint{}, footprintOf(t, pool, tenant),
				"no PENDING manifest, partial records or outbox event may survive")
		})
	}
}

func TestCompleteManifest_FailureAtFinalize_LeavesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	failWhen(t, pool, "inj_finalize", "evidence_manifests", "BEFORE UPDATE", "NEW.status = 'GENERATED'")

	_, err := s.CompleteManifest(ctx, newManifestInput(), recs("gd-1", "gd-2"), "corr-x")
	require.Error(t, err)
	assert.Equal(t, footprint{}, footprintOf(t, pool, tenant))
}

func TestCompleteManifest_FailureAtOutboxInsert_LeavesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	failWhen(t, pool, "inj_outbox", "outbox_events", "BEFORE INSERT", "NEW.event_type = 'evidence.manifest.generated'")

	_, err := s.CompleteManifest(ctx, newManifestInput(), recs("gd-1", "gd-2"), "corr-x")
	require.Error(t, err)
	assert.Equal(t, footprint{}, footprintOf(t, pool, tenant),
		"a GENERATED manifest without its event must not exist")
}

func TestCompleteManifest_MissingTenant_WritesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	before := footprintOf(t, pool, "00000000-0000-0000-0000-000000000000")
	_, err := s.CompleteManifest(context.Background(), newManifestInput(), recs("gd-1"), "corr-x")
	require.Error(t, err)
	assert.Equal(t, before, footprintOf(t, pool, "00000000-0000-0000-0000-000000000000"))
}

func TestRecordFailedManifest_IsOneFailedRow_NoRecords_NoEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)

	in := newManifestInput()
	got, err := s.RecordFailedManifest(ctx, in, "governance-svc unavailable")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, got.Status)
	require.NotNil(t, got.FailureReason)
	assert.Equal(t, "governance-svc unavailable", *got.FailureReason)
	assert.Equal(t, footprint{manifests: 1}, footprintOf(t, pool, tenant))

	var pending int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM evidence_manifests WHERE tenant_id=$1 AND status='PENDING'`, tenant).Scan(&pending))
	assert.Zero(t, pending, "no PENDING manifest may be left behind")
}

// ── Gap 7: deterministic record ordering ─────────────────────────────────────

// Every record written in one transaction shares fetched_at (now() is the
// transaction start), so fetched_at alone cannot order them.
func TestListRecords_IdenticalFetchedAt_OrderedByRecordID_Stably(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)

	got, err := s.CompleteManifest(ctx, newManifestInput(), recs("a", "b", "c", "d", "e", "f", "g", "h"), "corr-order")
	require.NoError(t, err)

	first, err := s.ListRecords(ctx, got.ManifestID)
	require.NoError(t, err)
	require.Len(t, first, 8)
	for _, r := range first {
		assert.True(t, r.FetchedAt.Equal(first[0].FetchedAt), "precondition: every record has the same fetched_at")
	}
	ids := make([]string, len(first))
	for i, r := range first {
		ids[i] = r.ManifestRecordID
	}
	assert.True(t, sort.StringsAreSorted(ids), "ties on fetched_at must break on manifest_record_id: %v", ids)

	for i := 0; i < 5; i++ {
		again, err := s.ListRecords(ctx, got.ManifestID)
		require.NoError(t, err)
		require.Equal(t, first, again, "repeated reads must return the identical order")
	}
}

// Inserted in the opposite order from the ids and with an explicit identical
// fetched_at: only the secondary key can produce the expected order.
func TestListRecords_ExplicitTie_InsertOrderDoesNotMatter(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)

	m := newManifestInput()
	require.NoError(t, s.CreateManifest(ctx, m))
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	idsDesc := []string{
		"ffffffff-0000-0000-0000-000000000003",
		"88888888-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-000000000001",
	}
	for i, id := range idsDesc {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO manifest_records (manifest_record_id, manifest_id, source_type, source_record_id, record_snapshot, fetched_at)
			VALUES ($1,$2,'GOVERNANCE_DECISION',$3,'{}',$4)`, id, m.ManifestID, fmt.Sprintf("src-%d", i), at)
		require.NoError(t, err)
	}
	got, err := s.ListRecords(ctx, m.ManifestID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "00000000-0000-0000-0000-000000000001", got[0].ManifestRecordID)
	assert.Equal(t, "88888888-0000-0000-0000-000000000002", got[1].ManifestRecordID)
	assert.Equal(t, "ffffffff-0000-0000-0000-000000000003", got[2].ManifestRecordID)
}
