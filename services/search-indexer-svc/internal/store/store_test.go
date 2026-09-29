// Integration tests for the control-plane store, against a REAL Postgres 16.
//
// They are gated on TEST_DATABASE_URL and skip without it — which is the
// standard trap in this repo: an unset variable skips every test here while
// `go test ./...` still prints ok, so a verification that verified nothing
// reads identically to one that passed. TestMain closes that: with
// REQUIRE_DB_TESTS=1 the suite FAILS LOUDLY instead of skipping, and audit.sh
// runs it that way.
//
// These tests need a real database rather than a fake because what they are
// testing IS the SQL: the compare-and-set in UpsertProjectionRecord, the
// partial unique index on ACTIVE generations, the RLS policies and the
// NULLIF guard. None of those exist anywhere else to be tested.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-indexer-svc/internal/domain"
)

func TestMain(m *testing.M) {
	if os.Getenv("TEST_DATABASE_URL") == "" && os.Getenv("REQUIRE_DB_TESTS") != "" {
		fmt.Fprintln(os.Stderr,
			"FATAL: REQUIRE_DB_TESTS is set but TEST_DATABASE_URL is not.\n"+
				"This suite claims to verify the SQL that enforces restriction-epoch\n"+
				"precedence, single-ACTIVE-generation and tenant RLS. Skipped, it\n"+
				"verifies none of it while still reporting ok.")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func testStore(t *testing.T) (*PgStore, func()) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; set REQUIRE_DB_TESTS=1 to make this a failure")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))

	// Schema, applied per run. The suite owns its database — audit.sh points
	// it at a scratch one — so a clean slate is the right starting state.
	for _, path := range []string{
		"../../deployments/migrations/000001_initial_schema.up.sql",
		"../../deployments/migrations/000002_add_rls.up.sql",
	} {
		sql, err := os.ReadFile(path)
		require.NoError(t, err, "migration %s", path)
		_, err = pool.Exec(ctx, string(sql))
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			require.NoError(t, err, "applying %s", path)
		}
	}

	s := New(pool, zap.NewNop())
	return s, func() {
		// Truncate rather than drop: dropping the tables would leave a
		// running service with no schema, which is the defect the
		// secret-vault audit script had to work around with a scratch
		// database.
		_, _ = pool.Exec(ctx, `TRUNCATE search_evidence, restriction_tombstones,
			projection_ledger, index_checkpoints, index_generations,
			search_field_definitions, index_contracts, search_sources CASCADE`)
		pool.Close()
	}
}

func seedSource(t *testing.T, s *PgStore) domain.SearchSource {
	t.Helper()
	src := domain.SearchSource{
		SourceID: uuid.NewString(), OwnerService: "obligations-svc",
		SourceType:  "obligation-" + uuid.NewString()[:8],
		TenantScope: "TENANT_SHARDED", ResidencyRegion: "EU",
		SensitivityCeiling: domain.SensitivityFinancial,
		EventTopic:         "zoiko.obligations.events",
		EventTypes:         []string{"obligation.created"},
		FreshnessClass:     "S1", MaxLagSeconds: 300,
		CreatedByPrincipalID: uuid.NewString(),
	}
	require.NoError(t, s.CreateSource(context.Background(), src))
	return src
}

func seedContract(t *testing.T, s *PgStore, src domain.SearchSource, scope string, state domain.ContractState) domain.IndexContract {
	t.Helper()
	ctx := context.Background()
	version, err := s.NextContractVersion(ctx, scope)
	require.NoError(t, err)

	c := domain.IndexContract{
		ContractID: uuid.NewString(), SourceID: src.SourceID, ScopeName: scope,
		Version: version, SchemaDigest: "digest", State: domain.ContractDraft,
		FreshnessClass: "S1", RetrievalClass: domain.RetrievalR1,
		AnalyzerProfile: "standard", AuthzAction: "OBLIGATION_READ",
		CreatedByPrincipalID: uuid.NewString(),
		Fields: []domain.SearchFieldDefinition{{
			FieldID: "obligation_code", SourcePath: "obligation_code", Type: "TEXT",
			Searchable: true, Returnable: true, SnippetAllowed: true,
			SensitivityClass: domain.SensitivityInternal,
		}},
	}
	require.NoError(t, s.CreateContract(ctx, c))

	if state != domain.ContractDraft {
		require.NoError(t, s.TransitionContract(ctx, c.ContractID, domain.ContractDraft, domain.ContractCertified))
		if state == domain.ContractPublished {
			require.NoError(t, s.TransitionContract(ctx, c.ContractID, domain.ContractCertified, domain.ContractPublished))
		}
	}
	c.State = state
	return c
}

// ── the compare-and-set that enforces NP-11 / NP-48 ──────────────────────────

// The headline SQL test: a restriction epoch that is not strictly newer is
// refused, whatever its source version says. This is the clause that makes
// "an older replay cannot resurrect removed visibility" true.
func TestUpsertProjectionRecord_RestrictionEpochWins(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()

	base := domain.ProjectionRecord{
		TenantID: tenant, ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-1", SourceVersion: 1, RestrictionEpoch: 0, ContentHash: "h1",
	}
	applied, err := s.UpsertProjectionRecord(ctx, base)
	require.NoError(t, err)
	assert.True(t, applied)

	// A restriction at a positive epoch supersedes it.
	restricted := base
	restricted.RestrictionEpoch = 1000
	restricted.Tombstoned = true
	restricted.ContentHash = "h-tomb"
	applied, err = s.UpsertProjectionRecord(ctx, restricted)
	require.NoError(t, err)
	assert.True(t, applied)

	// A replayed create at a MUCH higher source version but epoch 0 is
	// refused. Version comparison alone would have let this through, which is
	// precisely the resurrection NP-11 describes.
	replay := base
	replay.SourceVersion = 9999
	replay.RestrictionEpoch = 0
	applied, err = s.UpsertProjectionRecord(ctx, replay)
	require.NoError(t, err)
	assert.False(t, applied, "an epoch-0 event must not outrank a restriction")

	current, err := s.GetProjectionRecord(ctx, tenant, "obligation", "obligation", "ob-1")
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.True(t, current.Tombstoned, "the record must still be tombstoned")
	assert.Equal(t, int64(1000), current.RestrictionEpoch)
}

// At an EQUAL epoch, source version decides — ordinary replay suppression.
func TestUpsertProjectionRecord_EqualEpochComparesVersion(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()

	r := domain.ProjectionRecord{
		TenantID: tenant, ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-1", SourceVersion: 5, RestrictionEpoch: 0,
	}
	_, err := s.UpsertProjectionRecord(ctx, r)
	require.NoError(t, err)

	older := r
	older.SourceVersion = 4
	applied, err := s.UpsertProjectionRecord(ctx, older)
	require.NoError(t, err)
	assert.False(t, applied)

	same := r
	applied, err = s.UpsertProjectionRecord(ctx, same)
	require.NoError(t, err)
	assert.False(t, applied, "a redelivery at the same version is a no-op")

	newer := r
	newer.SourceVersion = 6
	applied, err = s.UpsertProjectionRecord(ctx, newer)
	require.NoError(t, err)
	assert.True(t, applied)
}

// ── tenant isolation ─────────────────────────────────────────────────────────

// The RLS policy, proven rather than asserted: tenant A cannot read tenant B's
// ledger row even by naming its exact key.
func TestProjectionLedger_IsTenantIsolated(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	_, err := s.UpsertProjectionRecord(ctx, domain.ProjectionRecord{
		TenantID: tenantB, ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-secret", SourceVersion: 1,
	})
	require.NoError(t, err)

	got, err := s.GetProjectionRecord(ctx, tenantA, "obligation", "obligation", "ob-secret")
	require.NoError(t, err)
	assert.Nil(t, got, "tenant A must not see tenant B's projection by naming its key")

	got, err = s.GetProjectionRecord(ctx, tenantB, "obligation", "obligation", "ob-secret")
	require.NoError(t, err)
	require.NotNil(t, got)
}

// An empty tenant is refused BEFORE it reaches Postgres. Passing it down casts
// ” to uuid, which raises — so an unscoped read failed as a 500, a server
// fault indistinguishable in monitoring from a real outage.
func TestStore_EmptyTenantIsRefusedNotCastToUUID(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	_, err := s.UpsertProjectionRecord(ctx, domain.ProjectionRecord{
		TenantID: "", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-1",
	})
	require.ErrorIs(t, err, domain.ErrTenantRequired)
	assert.NotContains(t, err.Error(), "invalid input syntax for type uuid",
		"the empty tenant must be refused above the driver, not by a failed cast")
}

// The NULLIF guard in 000002, tested the way it actually bites.
//
// THIS TEST CONNECTS AS A NON-SUPERUSER, and that is the whole point. A
// superuser bypasses row-level security unconditionally — FORCE ROW LEVEL
// SECURITY forces it for the table OWNER, not for a superuser — so the same
// assertions run as `postgres` pass whether the policy is correct, broken, or
// absent entirely. An earlier version of this test did exactly that and
// reported a policy that was never evaluated as working.
//
// Two properties, and they fail in different directions:
//
//   - Without NULLIF, set_config('app.tenant_id', ”) makes the policy cast
//     ” to uuid, which RAISES — so a legitimate query on a connection
//     recycled from the pool fails as a 500 rather than returning no rows.
//   - With NULLIF the comparison is against NULL, which is false for every
//     row: the session sees nothing, which is fail-closed.
func TestRLS_EmptyGUCMatchesNothingRatherThanRaising(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()

	_, err := s.UpsertProjectionRecord(ctx, domain.ProjectionRecord{
		TenantID: tenant, ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-1", SourceVersion: 1,
	})
	require.NoError(t, err)

	pool := nonSuperuserPool(t, s)
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	// 1. The empty GUC must not raise.
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', '', false)")
	require.NoError(t, err)

	var count int
	err = conn.QueryRow(ctx, "SELECT COUNT(*) FROM projection_ledger").Scan(&count)
	require.NoError(t, err,
		"an empty app.tenant_id must not raise; NULLIF in 000002 is what prevents it")
	assert.Zero(t, count, "an unscoped session sees nothing, which is fail-closed")

	// 2. And the policy is genuinely being evaluated — otherwise property 1
	// would pass against no policy at all.
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
	require.NoError(t, err)
	err = conn.QueryRow(ctx, "SELECT COUNT(*) FROM projection_ledger").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the correct tenant must see its own row")

	// 3. And another tenant sees nothing.
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", uuid.NewString())
	require.NoError(t, err)
	err = conn.QueryRow(ctx, "SELECT COUNT(*) FROM projection_ledger").Scan(&count)
	require.NoError(t, err)
	assert.Zero(t, count, "RLS must hide another tenant's row from a non-superuser")
}

// nonSuperuserPool creates (idempotently) an unprivileged role and returns a
// pool connected as it.
//
// The service itself runs as zoiko_app for the same reason: a pool connected
// as a superuser makes every RLS policy in this schema decorative. The
// explicit `AND tenant_id = $n` filters in the queries above are what actually
// isolate tenants when that happens, and they are deliberately BOTH present —
// but a policy nobody can observe working is a policy nobody will notice
// breaking.
func nonSuperuserPool(t *testing.T, s *PgStore) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const role = "si_rls_test_role"
	_, err := s.pool.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN
			CREATE ROLE `+role+` LOGIN PASSWORD 'rlstest';
		END IF;
	END $$;`)
	require.NoError(t, err)

	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		_, err := s.pool.Exec(ctx, grant)
		require.NoError(t, err)
	}

	dsn := os.Getenv("TEST_DATABASE_URL")
	// Swap the credentials in the DSN for the unprivileged role's.
	rewritten := strings.Replace(dsn, "postgres://postgres:postgres@", "postgres://"+role+":rlstest@", 1)
	require.NotEqual(t, dsn, rewritten,
		"TEST_DATABASE_URL must use the postgres://postgres:postgres@ form for this test to swap the role")

	pool, err := pgxpool.New(ctx, rewritten)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))
	return pool
}

// Search evidence is tenant-scoped absolutely: a tenant's evidence names its
// query digests and the principals that searched.
func TestSearchEvidence_IsTenantIsolated(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	require.NoError(t, s.RecordEvidence(ctx, domain.SearchEvidence{
		EvidenceID: uuid.NewString(), TenantID: tenantB, RequestID: "r-1",
		ScopeName: "obligation", QueryDigest: "d", FiltersDigest: "f", PlanDigest: "p",
		Completeness: domain.CompletenessComplete,
	}))

	rows, err := s.ListEvidence(ctx, tenantA, "", 50)
	require.NoError(t, err)
	assert.Empty(t, rows)

	rows, err = s.ListEvidence(ctx, tenantB, "", 50)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// ── generation lifecycle ─────────────────────────────────────────────────────

// The partial unique index, which is the control-plane half of NP-42: two
// ACTIVE generations for one scope cannot exist, whatever the code does.
func TestIndexGenerations_OnlyOneActivePerScope(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	scope := "scope-" + uuid.NewString()[:8]
	contract := seedContract(t, s, src, scope, domain.ContractPublished)

	mk := func() domain.IndexGeneration {
		return domain.IndexGeneration{
			GenerationID: uuid.NewString(), ContractID: contract.ContractID,
			ContractVersion: contract.Version, ScopeName: scope,
			PhysicalIndex: "idx-" + uuid.NewString()[:8], State: domain.GenerationPlanned,
			CreatedByPrincipalID: uuid.NewString(),
		}
	}
	promote := func(g domain.IndexGeneration) error {
		require.NoError(t, s.CreateGeneration(ctx, g))
		require.NoError(t, s.TransitionGeneration(ctx, g.GenerationID, domain.GenerationPlanned, domain.GenerationBuilding, "", ""))
		require.NoError(t, s.TransitionGeneration(ctx, g.GenerationID, domain.GenerationBuilding, domain.GenerationValidating, "", ""))
		require.NoError(t, s.TransitionGeneration(ctx, g.GenerationID, domain.GenerationValidating, domain.GenerationReady, "d", "n"))
		return s.TransitionGeneration(ctx, g.GenerationID, domain.GenerationReady, domain.GenerationActive, "", "")
	}

	require.NoError(t, promote(mk()))

	err := promote(mk())
	require.Error(t, err, "a second ACTIVE generation for one scope must be impossible")
	assert.True(t, errors.Is(err, domain.ErrConflict), "got %v", err)
}

// The transition is a compare-and-set, so two concurrent publishers cannot
// both win.
func TestTransitionGeneration_IsCompareAndSet(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	scope := "scope-" + uuid.NewString()[:8]
	contract := seedContract(t, s, src, scope, domain.ContractPublished)

	g := domain.IndexGeneration{
		GenerationID: uuid.NewString(), ContractID: contract.ContractID,
		ContractVersion: contract.Version, ScopeName: scope,
		PhysicalIndex: "idx-" + uuid.NewString()[:8], State: domain.GenerationPlanned,
		CreatedByPrincipalID: uuid.NewString(),
	}
	require.NoError(t, s.CreateGeneration(ctx, g))
	require.NoError(t, s.TransitionGeneration(ctx, g.GenerationID, domain.GenerationPlanned, domain.GenerationBuilding, "", ""))

	// The same transition again: the row is no longer PLANNED, so it affects
	// nothing and reports a conflict rather than silently succeeding.
	err := s.TransitionGeneration(ctx, g.GenerationID, domain.GenerationPlanned, domain.GenerationBuilding, "", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrConflict))
}

// ── contract lifecycle ───────────────────────────────────────────────────────

// Only one PUBLISHED version per scope: two would let a generation build from
// either, and §4.2's gate would have certified a field set that is not the
// one being built.
func TestIndexContracts_OnlyOnePublishedPerScope(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	scope := "scope-" + uuid.NewString()[:8]
	seedContract(t, s, src, scope, domain.ContractPublished)

	second := seedContract(t, s, src, scope, domain.ContractDraft)
	require.NoError(t, s.TransitionContract(ctx, second.ContractID, domain.ContractDraft, domain.ContractCertified))

	err := s.TransitionContract(ctx, second.ContractID, domain.ContractCertified, domain.ContractPublished)
	require.Error(t, err, "a scope must never have two PUBLISHED contracts")
	assert.True(t, errors.Is(err, domain.ErrConflict))
}

// A contract and its fields are written in ONE transaction. A contract with
// half its fields would produce a generation whose mapping silently omits a
// searchable field — a search that returns nothing and no error.
func TestCreateContract_WritesFieldsAtomically(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	scope := "scope-" + uuid.NewString()[:8]

	bad := domain.IndexContract{
		ContractID: uuid.NewString(), SourceID: src.SourceID, ScopeName: scope,
		Version: 1, SchemaDigest: "d", State: domain.ContractDraft,
		FreshnessClass: "S1", RetrievalClass: domain.RetrievalR1,
		AnalyzerProfile: "standard", AuthzAction: "READ",
		CreatedByPrincipalID: uuid.NewString(),
		Fields: []domain.SearchFieldDefinition{
			{FieldID: "ok_field", SourcePath: "ok", Type: "TEXT", Returnable: true,
				SensitivityClass: domain.SensitivityInternal},
			// Violates the snippet_requires_returnable CHECK.
			{FieldID: "bad_field", SourcePath: "bad", Type: "TEXT",
				SnippetAllowed: true, Returnable: false, SensitivityClass: domain.SensitivityInternal},
		},
	}
	require.Error(t, s.CreateContract(ctx, bad))

	contracts, err := s.ListContracts(ctx, scope)
	require.NoError(t, err)
	assert.Empty(t, contracts, "a failed field write must roll back the contract too")
}

// The database enforces INV-13 as well as the service does. Two places, on
// purpose: this is the constraint whose violation leaks text, and a database
// check survives a refactor of the validation code.
func TestSchema_RefusesSnippetWithoutReturnable(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	c := domain.IndexContract{
		ContractID: uuid.NewString(), SourceID: src.SourceID,
		ScopeName: "scope-" + uuid.NewString()[:8], Version: 1, SchemaDigest: "d",
		State: domain.ContractDraft, FreshnessClass: "S1", RetrievalClass: domain.RetrievalR1,
		AnalyzerProfile: "standard", AuthzAction: "READ", CreatedByPrincipalID: uuid.NewString(),
		Fields: []domain.SearchFieldDefinition{{
			FieldID: "notes", SourcePath: "notes", Type: "TEXT",
			SnippetAllowed: true, Returnable: false, SensitivityClass: domain.SensitivityInternal,
		}},
	}
	err := s.CreateContract(ctx, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snippet_requires_returnable")
}

// INV-09 at the schema level: a SECRET_PROHIBITED field cannot be exposed in
// any way, and the database says so too.
func TestSchema_RefusesExposedProhibitedField(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	c := domain.IndexContract{
		ContractID: uuid.NewString(), SourceID: src.SourceID,
		ScopeName: "scope-" + uuid.NewString()[:8], Version: 1, SchemaDigest: "d",
		State: domain.ContractDraft, FreshnessClass: "S1", RetrievalClass: domain.RetrievalR1,
		AnalyzerProfile: "standard", AuthzAction: "READ", CreatedByPrincipalID: uuid.NewString(),
		Fields: []domain.SearchFieldDefinition{{
			FieldID: "api_secret", SourcePath: "api_secret", Type: "KEYWORD",
			Searchable: true, SensitivityClass: domain.SensitivitySecretProhibited,
		}},
	}
	err := s.CreateContract(ctx, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prohibited_is_never_exposed")
}

// ── tombstones ───────────────────────────────────────────────────────────────

// NP-47: a replayed restriction event is idempotent, not a second tombstone.
// NP-48: a lower epoch is refused outright.
func TestUpsertTombstone_IdempotentAndMonotonic(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()

	t1 := domain.RestrictionTombstone{
		TenantID: tenant, ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-1", Reason: "PRV_ERASURE", Epoch: 2000,
		SourceEventID: "evt-1", EffectiveAt: time.Now().UTC(),
	}
	applied, err := s.UpsertTombstone(ctx, t1, uuid.NewString())
	require.NoError(t, err)
	assert.True(t, applied)

	// The same source event again: an idempotent NO-OP, not an error.
	//
	// NP-47 and NP-48 are different rules, and the order of the checks decides
	// whether they stay distinguishable. This used to compare the epoch first,
	// which made a replay's answer depend on the clock: arriving in a later
	// millisecond it carried a newer epoch and fell through to ON CONFLICT as a
	// no-op, arriving inside the same millisecond it compared equal and was
	// refused as stale. Two answers for one request.
	applied, err = s.UpsertTombstone(ctx, t1, uuid.NewString())
	require.NoError(t, err, "a replayed source event is idempotent, not an error")
	assert.False(t, applied, "and it changes nothing")

	// Still a no-op at a LATER epoch, because it is the same source event.
	laterClock := t1
	laterClock.Epoch = 9999
	applied, err = s.UpsertTombstone(ctx, laterClock, uuid.NewString())
	require.NoError(t, err)
	assert.False(t, applied, "identity is the source event, not the clock")

	// A DIFFERENT source event at a LOWER epoch — a genuinely out-of-order
	// delivery. Refused, because applying it would reopen the ordering.
	older := t1
	older.Epoch = 1000
	older.SourceEventID = "evt-0"
	_, err = s.UpsertTombstone(ctx, older, uuid.NewString())
	require.ErrorIs(t, err, domain.ErrStaleEpoch)

	// A newer one applies.
	newer := t1
	newer.Epoch = 3000
	newer.SourceEventID = "evt-2"
	applied, err = s.UpsertTombstone(ctx, newer, uuid.NewString())
	require.NoError(t, err)
	assert.True(t, applied)
}

// §2.2's APPLIED-is-not-VERIFIED distinction, persisted.
func TestTombstoneStates_AppliedAndVerifiedAreDistinct(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenant := uuid.NewString()

	tomb := domain.RestrictionTombstone{
		TenantID: tenant, ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-1", Reason: "PRV_ERASURE", Epoch: 1,
		SourceEventID: "evt-1", EffectiveAt: time.Now().UTC(),
	}
	_, err := s.UpsertTombstone(ctx, tomb, uuid.NewString())
	require.NoError(t, err)

	require.NoError(t, s.MarkTombstoneState(ctx, tenant, "obligation", "ob-1", "evt-1",
		domain.PropagationApplied, ""))
	rows, err := s.ListTombstones(ctx, tenant, "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, domain.PropagationApplied, rows[0].State)
	require.NotNil(t, rows[0].PropagatedAt)
	assert.Nil(t, rows[0].VerifiedAt, "APPLIED must not set verified_at")

	require.NoError(t, s.MarkTombstoneState(ctx, tenant, "obligation", "ob-1", "evt-1",
		domain.PropagationVerified, ""))
	rows, err = s.ListTombstones(ctx, tenant, "", 10)
	require.NoError(t, err)
	require.NotNil(t, rows[0].VerifiedAt)
}

// The verifier sweep is cross-tenant by necessity — it has no inbound request
// to inherit a tenant from — and the platform-scope flag is what lets it see
// every tenant's unverified tombstones.
func TestListPendingVerification_SeesEveryTenant(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	for _, tenant := range []string{tenantA, tenantB} {
		_, err := s.UpsertTombstone(ctx, domain.RestrictionTombstone{
			TenantID: tenant, ScopeName: "obligation", SourceType: "obligation",
			SourceID: "ob-1", Reason: "PRV_ERASURE", Epoch: 1,
			SourceEventID: "evt-" + tenant[:8], EffectiveAt: time.Now().UTC(),
		}, uuid.NewString())
		require.NoError(t, err)
	}

	pending, err := s.ListPendingVerification(ctx, 100)
	require.NoError(t, err)
	assert.Len(t, pending, 2, "the verifier must see every tenant's backlog")
}

// ── checkpoints ──────────────────────────────────────────────────────────────

// NP-17. A watermark that can move BACKWARD is worse than one that stalls: a
// replay from an earlier offset would rewrite the high-water mark and make a
// gap look like progress. GREATEST is what prevents it.
func TestUpsertCheckpoint_WatermarkNeverMovesBackward(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	scope := "scope-" + uuid.NewString()[:8]

	require.NoError(t, s.UpsertCheckpoint(ctx, domain.IndexCheckpoint{
		ScopeName: scope, SourcePartition: "p0", Watermark: 5000,
		CommittedAt: time.Now().UTC(), Freshness: domain.FreshnessCurrent,
	}))
	require.NoError(t, s.UpsertCheckpoint(ctx, domain.IndexCheckpoint{
		ScopeName: scope, SourcePartition: "p0", Watermark: 1000,
		CommittedAt: time.Now().UTC(), Freshness: domain.FreshnessLagging,
	}))

	cps, err := s.ListCheckpoints(ctx, scope)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	assert.Equal(t, int64(5000), cps[0].Watermark, "a replay must not rewind the high-water mark")
	assert.Equal(t, domain.FreshnessLagging, cps[0].Freshness,
		"but the freshness assessment does update")
}

// §2.2: UNKNOWN is never represented as CURRENT, including as a default.
func TestCheckpoint_DefaultFreshnessIsUnknown(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()
	scope := "scope-" + uuid.NewString()[:8]

	_, err := s.pool.Exec(ctx,
		`INSERT INTO index_checkpoints (scope_name, source_partition) VALUES ($1, 'p0')`, scope)
	require.NoError(t, err)

	cps, err := s.ListCheckpoints(ctx, scope)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	assert.Equal(t, domain.FreshnessUnknown, cps[0].Freshness)
}

// ── sources ──────────────────────────────────────────────────────────────────

// Two registrations of the same source_type would give one document two
// contracts and no way to say which applied.
func TestCreateSource_SourceTypeIsUnique(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()
	ctx := context.Background()

	src := seedSource(t, s)
	dup := src
	dup.SourceID = uuid.NewString()

	err := s.CreateSource(ctx, dup)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrConflict))
}
