package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// TestMain boots an embedded PostgreSQL once for the whole suite when
// EMBEDDED_PG_STORE_TESTS=1 and no TEST_DATABASE_URL is set, so the real
// migrations (including the 000009 deactivation/append-only triggers) can be
// exercised without a server. A live DB env is used as-is when present.
func TestMain(m *testing.M) {
	if os.Getenv("TEST_DATABASE_URL") == "" && os.Getenv("EMBEDDED_PG_STORE_TESTS") == "1" {
		if err := startEmbeddedPostgres(); err != nil {
			fmt.Fprintf(os.Stderr, "embedded postgres: %v\n", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	stopEmbeddedPostgres()
	os.Exit(code)
}

var (
	pgOnce     sync.Once
	pgStartErr error
	pgStopFn   func() error
)

func startEmbeddedPostgres() error {
	pgOnce.Do(func() {
		port := uint32(17301 + uint32(os.Getpid()%499))
		pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(port).
			Database("jur_store_test").
			Username("postgres").
			Password("postgres"))
		if err := pg.Start(); err != nil {
			pgStartErr = err
			return
		}
		pgStopFn = pg.Stop
		os.Setenv("TEST_DATABASE_URL", fmt.Sprintf(
			"host=localhost port=%d dbname=jur_store_test user=postgres password=postgres sslmode=disable", port))
	})
	return pgStartErr
}

func stopEmbeddedPostgres() {
	if pgStopFn != nil {
		_ = pgStopFn()
		pgStopFn = nil
	}
}

func getTestPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		// A SILENT SKIP HERE TURNS THE WHOLE STORE SUITE INTO NOTHING while
		// `go test ./...` still prints ok. Skip locally, where a developer
		// without Postgres is a normal state, and FAIL wherever the run claims
		// to be a verification. CI is set by GitHub Actions; REQUIRE_DB_TESTS
		// is the local opt-in for reproducing a certification run by hand.
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set, but CI or REQUIRE_DB_TESTS is: " +
				"this run claims to verify the store and would instead have skipped every test in it")
		}
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}
	return pool
}

// migrationFiles is every up migration the store suite needs, in order.
// Listing them here rather than naming two of them inline means a migration
// added later is applied by the tests automatically — the alternative has
// already produced a live incident in this repo, where a migration was never
// applied and every write failed with a 42P10 that read like a code bug.
//
// 000005-000006 (outbox, pack registries, pack artifacts) are deliberately
// not applied: the store suite only exercises jurisdictions,
// jurisdiction_rules and jurisdiction_rule_drift_events, and those pack
// tables are neither dropped nor asserted here. 000007/000008 are required —
// ruleColumnNames (pg_store.go) reads rule_version, supersedes_rule_id and
// precedence_level, and without them every CreateRule fails with 42703.
// 000009 must be applied because DeactivateJurisdiction's idempotency relies
// on trg_prevent_duplicate_deactivation (a second UPDATE is a no-op), and the
// drift append-only assertions rely on trg_enforce_drift_append_only.
// 000017 adds the rule_status_history DELETE guard asserted below plus the
// runtime-role privilege revokes.
// 000016 must follow 000007: 000007's status-history trigger passes
// NULL updated_at/updated_by on insert into NOT NULL columns, which makes
// every rule insert fail until 000016 replaces the function.
var migrationFiles = []string{
	"000001_initial_schema.up.sql",
	"000002_add_audit_columns.up.sql",
	"000003_add_data_classification.up.sql",
	"000004_add_rule_code_index.up.sql",
	"000007_add_bitemporal_replay.up.sql",
	"000008_add_precedence_metadata.up.sql",
	"000009_enforce_drift_append_only.up.sql",
	"000016_fix_rule_status_history_trigger.up.sql",
	"000017_append_only_rbac.up.sql",
}

// setupTestDB drops and recreates the schema from the migration files.
func setupTestDB(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// rule_status_history is listed so each test starts with a clean history
	// — 000007's CREATE TABLE IF NOT EXISTS would otherwise let rows from a
	// previous test survive (the FK to the dropped jurisdiction_rules is
	// removed by CASCADE, but the table and its rows are not).
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS rule_status_history, jurisdiction_rule_drift_events, jurisdiction_rules, jurisdictions CASCADE;")

	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")

	for _, name := range migrationFiles {
		sql, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to execute migration %s: %v", name, err)
		}
	}
}

// newTestStore returns a store against a freshly migrated schema.
func newTestStore(t *testing.T) (*store.PgStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := getTestPool(t)
	t.Cleanup(pool.Close)
	setupTestDB(t, pool)
	return store.New(pool, zap.NewNop()), pool, context.Background()
}

// mustCreateJurisdiction inserts a jurisdiction and fails the test if it cannot.
func mustCreateJurisdiction(t *testing.T, s *store.PgStore, ctx context.Context, code string, parent *string) *domain.Jurisdiction {
	t.Helper()
	jType := "COUNTRY"
	if parent != nil {
		jType = "STATE_PROVINCE"
	}
	j, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       uuid.New().String(),
		JurisdictionCode:     code,
		JurisdictionName:     "Jurisdiction " + code,
		JurisdictionType:     jType,
		ParentJurisdictionID: parent,
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC().Add(-365 * 24 * time.Hour),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create jurisdiction %s: %v", code, err)
	}
	return j
}

func ptr[T any](v T) *T { return &v }

// ── jurisdictions ────────────────────────────────────────────────────────────

func TestPgStore_CreateJurisdiction_IdempotencyAnd409(t *testing.T) {
	s, _, ctx := newTestStore(t)

	id := uuid.New().String()
	params := domain.CreateJurisdictionParams{
		JurisdictionID:       id,
		JurisdictionCode:     "US-CA",
		JurisdictionName:     "California",
		JurisdictionType:     "STATE_PROVINCE",
		AuthorityType:        "STATE",
		EffectiveFrom:        time.Now().UTC().Truncate(time.Microsecond),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	j1, created, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on create: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if j1.JurisdictionCode != "US-CA" {
		t.Errorf("expected code US-CA, got %s", j1.JurisdictionCode)
	}
	// PUBLIC is the tier data_classification_audit.md §2.11 assigns to
	// jurisdictions; it must be written without the caller supplying it.
	if j1.DataClassification != "PUBLIC" {
		t.Errorf("expected data_classification PUBLIC, got %q", j1.DataClassification)
	}

	// 2. Identical retry (idempotent 200 OK no-op)
	j2, created, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical retry")
	}
	if j2.JurisdictionID != j1.JurisdictionID {
		t.Errorf("expected ID %s, got %s", j1.JurisdictionID, j2.JurisdictionID)
	}

	// 3. Differing attribute on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.JurisdictionID = uuid.New().String()           // different ID, but same (code, type, parent)
	conflictParams.JurisdictionName = "California Republic State" // differing attribute!

	_, created, err = s.CreateJurisdiction(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing attribute, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

// TestPgStore_CreateJurisdiction_UnknownParent — the self-referential foreign
// key raised 23503, which the generic error path reported as
// ErrStoreUnavailable, i.e. an outage signal for a client mistake.
func TestPgStore_CreateJurisdiction_UnknownParent(t *testing.T) {
	s, _, ctx := newTestStore(t)

	_, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       uuid.New().String(),
		JurisdictionCode:     "US-CA",
		JurisdictionName:     "California",
		JurisdictionType:     "STATE_PROVINCE",
		ParentJurisdictionID: ptr(uuid.New().String()), // never inserted
		AuthorityType:        "STATE",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrParentNotFound) {
		t.Fatalf("expected ErrParentNotFound, got %v", err)
	}
}

// TestPgStore_DeactivateJurisdiction also asserts the record is end-dated.
// The domain model states deactivation is "active_flag + effective_to", but
// only active_flag was written, so a deactivated jurisdiction kept an
// open-ended effective period.
func TestPgStore_DeactivateJurisdiction(t *testing.T) {
	s, _, ctx := newTestStore(t)

	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	deactivated, changed, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "actor-deactivate")
	if err != nil {
		t.Fatalf("unexpected error on deactivate: %v", err)
	}
	if !changed {
		t.Error("first deactivate must report changed=true")
	}
	if deactivated.ActiveFlag {
		t.Errorf("expected ActiveFlag=false after deactivation")
	}
	if deactivated.EffectiveTo == nil {
		t.Error("expected EffectiveTo to be set — deactivation is active_flag + effective_to, not active_flag alone")
	}
	if deactivated.UpdatedAt == nil {
		t.Fatal("expected UpdatedAt to be set")
	}
	if deactivated.UpdatedByPrincipalID == nil || *deactivated.UpdatedByPrincipalID != "actor-deactivate" {
		t.Errorf("expected UpdatedByPrincipalID=actor-deactivate, got %v", deactivated.UpdatedByPrincipalID)
	}

	// A second deactivate is an idempotent replay: changed=false, the audit
	// columns are not rewritten and no re-announcement may be made.
	again, changedAgain, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "actor-again")
	if err != nil {
		t.Fatalf("unexpected error on replay deactivate: %v", err)
	}
	if changedAgain {
		t.Error("a replay deactivate must report changed=false")
	}
	if again.UpdatedByPrincipalID == nil || *again.UpdatedByPrincipalID != "actor-deactivate" {
		t.Errorf("replay must not rewrite updated_by; got %v", again.UpdatedByPrincipalID)
	}
	if again.EffectiveTo == nil || deactivated.EffectiveTo == nil || !again.EffectiveTo.Equal(*deactivated.EffectiveTo) {
		t.Errorf("replay must not move effective_to; got %v, want %v", again.EffectiveTo, deactivated.EffectiveTo)
	}

	// The active-only validation contract must now reject it.
	if _, err := s.FindByID(ctx, j.JurisdictionID); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected FindByID to 404 a deactivated jurisdiction, got %v", err)
	}
	// But it must still be readable for historical replay.
	if _, err := s.FindByIDAny(ctx, j.JurisdictionID); err != nil {
		t.Errorf("expected FindByIDAny to still find the deactivated jurisdiction, got %v", err)
	}

	// Non-existent deactivation
	_, _, err = s.DeactivateJurisdiction(ctx, uuid.New().String(), "actor-1")
	if !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected ErrJurisdictionNotFound for unknown UUID, got: %v", err)
	}
}

// TestPgStore_MalformedUUIDIsNotFoundNotOutage — a syntactically impossible
// id died in the pgx driver as SQLSTATE 22P02 and surfaced as 503
// store_unavailable, so a client typo read as a platform outage and made the
// fail-closed 503 contract meaningless.
func TestPgStore_MalformedUUIDIsNotFoundNotOutage(t *testing.T) {
	s, _, ctx := newTestStore(t)

	if _, err := s.FindByID(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindByID: expected ErrJurisdictionNotFound, got %v", err)
	}
	if _, err := s.FindByIDAny(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindByIDAny: expected ErrJurisdictionNotFound, got %v", err)
	}
	if _, err := s.FindRuleByID(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrRuleNotFound) {
		t.Errorf("FindRuleByID: expected ErrRuleNotFound, got %v", err)
	}
	if _, err := s.FindAncestors(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindAncestors: expected ErrJurisdictionNotFound, got %v", err)
	}
	if _, _, err := s.DeactivateJurisdiction(ctx, "not-a-uuid", "actor"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("DeactivateJurisdiction: expected ErrJurisdictionNotFound, got %v", err)
	}
}

// ── hierarchy ────────────────────────────────────────────────────────────────

// TestPgStore_FindAncestors_Chain verifies a country → state → authority
// chain resolves nearest-first in a single query.
func TestPgStore_FindAncestors_Chain(t *testing.T) {
	s, _, ctx := newTestStore(t)

	country := mustCreateJurisdiction(t, s, ctx, "US", nil)
	state := mustCreateJurisdiction(t, s, ctx, "US-CA", &country.JurisdictionID)
	authority := mustCreateJurisdiction(t, s, ctx, "US-CA-FTB", &state.JurisdictionID)

	ancestors, err := s.FindAncestors(ctx, authority.JurisdictionID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ancestors) != 2 {
		t.Fatalf("expected 2 ancestors, got %d", len(ancestors))
	}
	if ancestors[0].JurisdictionID != state.JurisdictionID {
		t.Errorf("expected nearest ancestor to be the state, got %s", ancestors[0].JurisdictionCode)
	}
	if ancestors[1].JurisdictionID != country.JurisdictionID {
		t.Errorf("expected root ancestor to be the country, got %s", ancestors[1].JurisdictionCode)
	}

	// A root jurisdiction has no ancestors, but is not "not found".
	rootAncestors, err := s.FindAncestors(ctx, country.JurisdictionID)
	if err != nil {
		t.Fatalf("unexpected error for root: %v", err)
	}
	if len(rootAncestors) != 0 {
		t.Errorf("expected 0 ancestors for a root jurisdiction, got %d", len(rootAncestors))
	}

	if _, err := s.FindAncestors(ctx, uuid.New().String()); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected ErrJurisdictionNotFound for an unknown id, got %v", err)
	}
}

// TestPgStore_FindAncestors_CycleTerminates — the old iterative walk had no
// visited set, so a cycle returned the same jurisdictions maxAncestorDepth
// times instead of terminating. The cycle is created with direct SQL because
// CreateJurisdiction refuses to build one.
func TestPgStore_FindAncestors_CycleTerminates(t *testing.T) {
	s, pool, ctx := newTestStore(t)

	a := mustCreateJurisdiction(t, s, ctx, "CYC-A", nil)
	b := mustCreateJurisdiction(t, s, ctx, "CYC-B", &a.JurisdictionID)

	// Close the loop: A's parent becomes B.
	if _, err := pool.Exec(ctx,
		"UPDATE jurisdictions SET parent_jurisdiction_id = $1 WHERE jurisdiction_id = $2",
		b.JurisdictionID, a.JurisdictionID,
	); err != nil {
		t.Fatalf("failed to create cycle: %v", err)
	}

	ancestors, err := s.FindAncestors(ctx, a.JurisdictionID)
	if err != nil {
		t.Fatalf("unexpected error walking a cyclic hierarchy: %v", err)
	}
	// A → B, and B's parent A is already seen, so the walk stops there.
	if len(ancestors) != 1 {
		t.Fatalf("expected the walk to stop at the repeat (1 ancestor), got %d", len(ancestors))
	}
	if ancestors[0].JurisdictionID != b.JurisdictionID {
		t.Errorf("expected ancestor %s, got %s", b.JurisdictionID, ancestors[0].JurisdictionID)
	}
}

func TestPgStore_CreateJurisdiction_RejectsSelfParent(t *testing.T) {
	s, _, ctx := newTestStore(t)

	id := uuid.New().String()
	_, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       id,
		JurisdictionCode:     "SELF",
		JurisdictionName:     "Self Parent",
		JurisdictionType:     "COUNTRY",
		ParentJurisdictionID: &id,
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrCyclicHierarchy) {
		t.Fatalf("expected ErrCyclicHierarchy, got %v", err)
	}
}

// ── rules ────────────────────────────────────────────────────────────────────

func TestPgStore_CreateRule_IdempotencyAnd409(t *testing.T) {
	s, _, ctx := newTestStore(t)

	j := mustCreateJurisdiction(t, s, ctx, "DE", nil)

	ruleID := uuid.New().String()
	effFrom := time.Now().UTC().Truncate(time.Microsecond)
	params := domain.CreateRuleParams{
		JurisdictionRuleID:   ruleID,
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "DE_VAT_STANDARD",
		RuleName:             "Standard VAT Rate",
		EffectiveFrom:        effFrom,
		RulePayload:          []byte(`{"filing_frequency": "MONTHLY"}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	r1, created, err := s.CreateRule(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error creating rule: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if r1.RuleCode != "DE_VAT_STANDARD" {
		t.Errorf("expected code DE_VAT_STANDARD, got %s", r1.RuleCode)
	}
	if r1.DataClassification != "INTERNAL" {
		t.Errorf("expected data_classification INTERNAL, got %q", r1.DataClassification)
	}

	// 2. Identical retry (idempotent 200 OK no-op).
	// JSONB normalises key order and whitespace on the way out, so this also
	// covers the byte-comparison bug that turned a retried POST into a 409.
	r2, created, err := s.CreateRule(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on retry")
	}
	if r2.JurisdictionRuleID != r1.JurisdictionRuleID {
		t.Errorf("expected ID %s, got %s", r1.JurisdictionRuleID, r2.JurisdictionRuleID)
	}

	// 3. Differing payload on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.JurisdictionRuleID = uuid.New().String()
	conflictParams.RulePayload = []byte(`{"filing_frequency": "QUARTERLY"}`) // differing payload!

	_, created, err = s.CreateRule(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing payload, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

// TestPgStore_CreateRule_UnknownJurisdiction — this hit the foreign key and
// surfaced as ErrStoreUnavailable, so "you named a jurisdiction that does not
// exist" and "the database is down" were the same response.
func TestPgStore_CreateRule_UnknownJurisdiction(t *testing.T) {
	s, _, ctx := newTestStore(t)

	_, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID:       uuid.New().String(),
		RuleDomain:           "TAX",
		RuleCode:             "X",
		RuleName:             "X",
		EffectiveFrom:        time.Now().UTC(),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "DRAFT",
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound, got %v", err)
	}
}

// TestPgStore_CreateRule_RejectsOverlap — two live rules with the same code
// and overlapping periods both satisfy a point-in-time query, which makes
// "the effective rule at date X" ambiguous.
func TestPgStore_CreateRule_RejectsOverlap(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "FR", nil)

	base := domain.CreateRuleParams{
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "FR_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:          ptr(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}
	if _, _, err := s.CreateRule(ctx, base); err != nil {
		t.Fatalf("failed to create the incumbent rule: %v", err)
	}

	// Starts inside the incumbent's period.
	overlapping := base
	overlapping.JurisdictionRuleID = uuid.New().String()
	overlapping.EffectiveFrom = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	overlapping.EffectiveTo = nil
	if _, _, err := s.CreateRule(ctx, overlapping); !errors.Is(err, domain.ErrOverlappingRule) {
		t.Fatalf("expected ErrOverlappingRule, got %v", err)
	}

	// Starts exactly when the incumbent ends — half-open intervals do not
	// overlap, so this is the legitimate successor and must be accepted.
	successor := base
	successor.JurisdictionRuleID = uuid.New().String()
	successor.EffectiveFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	successor.EffectiveTo = nil
	if _, _, err := s.CreateRule(ctx, successor); err != nil {
		t.Fatalf("an adjacent successor must be allowed, got %v", err)
	}

	// A DRAFT replacement may be prepared while the incumbent is in force.
	draft := base
	draft.JurisdictionRuleID = uuid.New().String()
	draft.EffectiveFrom = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	draft.EffectiveTo = nil
	draft.RuleStatus = "DRAFT"
	if _, _, err := s.CreateRule(ctx, draft); err != nil {
		t.Fatalf("a DRAFT overlapping the incumbent must be allowed, got %v", err)
	}

	// ...but activating it must not be, because that is when the ambiguity
	// would become real.
	_, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        draft.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "admin-1",
	})
	if !errors.Is(err, domain.ErrOverlappingRule) {
		t.Fatalf("expected activating an overlapping DRAFT to be refused, got %v", err)
	}
}

func TestPgStore_TransitionRuleStatus_StateMachineAndNoOp(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "FR", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "PAYROLL",
		RuleCode:             "FR_SOCIAL_SEC",
		RuleName:             "Social Security Contribution",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "DRAFT", // initial state
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	// 1. Legal transition DRAFT -> ACTIVE
	updated, transitioned, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "actor-1",
	})
	if err != nil {
		t.Fatalf("unexpected error transitioning DRAFT -> ACTIVE: %v", err)
	}
	if !transitioned {
		t.Error("expected transitioned=true for a real status change")
	}
	if updated.RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", updated.RuleStatus)
	}
	if updated.UpdatedAt == nil {
		t.Fatal("expected UpdatedAt to be set after transition")
	}
	// Activation must not close the rule.
	if updated.EffectiveTo != nil {
		t.Errorf("ACTIVE must not end-date the rule, got effective_to=%v", updated.EffectiveTo)
	}

	// 2. Idempotent network retry: call ACTIVE again when current is already ACTIVE.
	// allowedPriors is still ["DRAFT"] — without the pre-read check this fails.
	retried, transitioned, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "actor-1",
	})
	if err != nil {
		t.Fatalf("unexpected error on idempotent retry: %v", err)
	}
	if transitioned {
		t.Error("expected transitioned=false on a replay — the caller must not re-publish rule.activated")
	}
	if retried.RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE on retry, got %s", retried.RuleStatus)
	}

	// 3. Illegal transition: nothing moves back to DRAFT.
	_, _, err = s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "DRAFT",
		AllowedPriors: []string{},
		ActorID:       "actor-1",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition on illegal transition, got: %v", err)
	}
}

// TestPgStore_TransitionRuleStatus_SupersedeEndDates is the regression test
// for the ambiguity a superseded rule left behind: with effective_to still
// NULL it kept matching every point-in-time query, side by side with its own
// replacement.
func TestPgStore_TransitionRuleStatus_SupersedeEndDates(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "IE", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "IE_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	closedAt := time.Date(2025, 4, 6, 0, 0, 0, 0, time.UTC)
	superseded, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "SUPERSEDED",
		AllowedPriors: []string{"ACTIVE"},
		EndDate:       true,
		EffectiveTo:   &closedAt,
		ActorID:       "actor-1",
	})
	if err != nil {
		t.Fatalf("unexpected error superseding: %v", err)
	}
	if superseded.EffectiveTo == nil {
		t.Fatal("a superseded rule must be end-dated")
	}
	if !superseded.EffectiveTo.Equal(closedAt) {
		t.Errorf("effective_to = %v, want the supplied %v", superseded.EffectiveTo, closedAt)
	}

	// It must now be invisible to a query after the close date...
	after, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: j.JurisdictionID,
		EffectiveAt:    closedAt.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("expected the superseded rule to be excluded after its end date, got %d rules", len(after))
	}

	// ...and still visible to a historical one, which is what "historical
	// actions must always be explainable" requires.
	before, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: j.JurisdictionID,
		EffectiveAt:    time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected the superseded rule to remain visible historically, got %d rules", len(before))
	}
}

// TestPgStore_TransitionRuleStatus_RejectsEndDateBeforeStart — a period that
// ends before it starts can never match a point-in-time query.
func TestPgStore_TransitionRuleStatus_RejectsEndDateBeforeStart(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "NL", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "NL_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	_, _, err = s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "RETIRED",
		AllowedPriors: []string{"ACTIVE", "SUPERSEDED"},
		EndDate:       true,
		EffectiveTo:   ptr(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		ActorID:       "actor-1",
	})
	if !errors.Is(err, domain.ErrInvalidEffectivePeriod) {
		t.Fatalf("expected ErrInvalidEffectivePeriod, got %v", err)
	}
}

// ── legal drift ──────────────────────────────────────────────────────────────

// TestPgStore_RecordDrift covers the capability the drift_events table
// shipped for and nothing ever used: legal_drift_state had no history and no
// way to be set, despite legal.drift.detected being a published event.
func TestPgStore_RecordDrift(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "PAYROLL",
		RuleCode:             "GB_NI_THRESHOLD",
		RuleName:             "National Insurance Threshold",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}
	if r.LegalDriftState != "CURRENT" {
		t.Fatalf("expected a new rule to start CURRENT, got %q", r.LegalDriftState)
	}

	// 1. CURRENT -> DRIFTED, with evidence.
	reason := "HMRC SI 2025/412 revised the threshold"
	drifted, event, changed, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "DRIFTED",
		Reason:                &reason,
		RecordedByPrincipalID: "feed-worker-1",
		CorrelationID:         "corr-drift-1",
	})
	if err != nil {
		t.Fatalf("unexpected error recording drift: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for a real drift transition")
	}
	if drifted.LegalDriftState != "DRIFTED" {
		t.Errorf("rule drift state = %q, want DRIFTED", drifted.LegalDriftState)
	}
	if event == nil {
		t.Fatal("expected a drift event to be written")
	}
	if event.FromState != "CURRENT" || event.ToState != "DRIFTED" {
		t.Errorf("event transition = %s -> %s, want CURRENT -> DRIFTED", event.FromState, event.ToState)
	}
	if event.Reason == nil || *event.Reason != reason {
		t.Errorf("event reason = %v, want %q", event.Reason, reason)
	}
	if event.CorrelationID == nil || *event.CorrelationID != "corr-drift-1" {
		t.Errorf("event correlation_id = %v, want corr-drift-1", event.CorrelationID)
	}

	// 2. Replay must be a no-op and must NOT append a second history entry.
	_, replayEvent, changed, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "DRIFTED",
		RecordedByPrincipalID: "feed-worker-1",
	})
	if err != nil {
		t.Fatalf("unexpected error on replay: %v", err)
	}
	if changed {
		t.Error("expected changed=false on replay")
	}
	if replayEvent != nil {
		t.Error("a replay must not append to the append-only history")
	}

	// 3. DRIFTED -> UNDER_REVIEW -> CURRENT builds the full history.
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "UNDER_REVIEW",
		RecordedByPrincipalID: "reviewer-1",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "CURRENT",
		RecordedByPrincipalID: "reviewer-1",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	history, err := s.FindDriftEvents(ctx, r.JurisdictionRuleID, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error reading history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
	// Newest first.
	if history[0].ToState != "CURRENT" {
		t.Errorf("expected newest-first ordering, got %q first", history[0].ToState)
	}

	// Unknown rule.
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    uuid.New().String(),
		ToState:               "DRIFTED",
		RecordedByPrincipalID: "x",
	}); !errors.Is(err, domain.ErrRuleNotFound) {
		t.Errorf("expected ErrRuleNotFound, got %v", err)
	}
	if _, err := s.FindDriftEvents(ctx, uuid.New().String(), 0, 0); !errors.Is(err, domain.ErrRuleNotFound) {
		t.Errorf("expected ErrRuleNotFound from FindDriftEvents, got %v", err)
	}
}

// ── rule pack ────────────────────────────────────────────────────────────────

// TestPgStore_FindRulePack_ResolvesInheritance is the test for the capability
// that did not exist: callers had to fetch the ancestor chain and issue one
// /rules request per generation, then decide for themselves which rule wins.
func TestPgStore_FindRulePack_ResolvesInheritance(t *testing.T) {
	s, _, ctx := newTestStore(t)

	country := mustCreateJurisdiction(t, s, ctx, "US", nil)
	state := mustCreateJurisdiction(t, s, ctx, "US-CA", &country.JurisdictionID)

	// Rules are effective from 2024; the jurisdictions (helper) are effective
	// since now-365d, so any `at` at or after now lies inside every window.
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	at := time.Now().UTC()

	newRule := func(jID, domainName, code, name, status string, effFrom time.Time) {
		t.Helper()
		if _, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
			JurisdictionRuleID:   uuid.New().String(),
			JurisdictionID:       jID,
			RuleDomain:           domainName,
			RuleCode:             code,
			RuleName:             name,
			EffectiveFrom:        effFrom,
			RulePayload:          []byte(`{}`),
			RuleStatus:           status,
			CreatedByPrincipalID: "admin-1",
		}); err != nil {
			t.Fatalf("failed to create rule %s/%s: %v", code, name, err)
		}
	}

	// Federal rules, one of which the state overrides.
	newRule(country.JurisdictionID, "TAX", "FILING_FREQ", "Federal filing frequency", "ACTIVE", from)
	newRule(country.JurisdictionID, "TAX", "FEDERAL_ONLY", "Federal only", "ACTIVE", from)
	newRule(state.JurisdictionID, "TAX", "FILING_FREQ", "State filing frequency", "ACTIVE", from)
	// Excluded: a DRAFT and a RETIRED never enter a runtime pack.
	newRule(state.JurisdictionID, "TAX", "STATE_DRAFT", "Draft", "DRAFT", from)
	newRule(state.JurisdictionID, "TAX", "STATE_RETIRED", "Retired", "RETIRED", from)
	// Different domain, so it must be filtered out when domain=TAX.
	newRule(state.JurisdictionID, "PAYROLL", "STATE_PAYROLL", "State payroll", "ACTIVE", from)

	pack, err := s.FindRulePack(ctx, state.JurisdictionID, "TAX", at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pack.ResolvedFrom) != 2 || pack.ResolvedFrom[0] != state.JurisdictionID || pack.ResolvedFrom[1] != country.JurisdictionID {
		t.Errorf("resolved_from = %v, want [state country]", pack.ResolvedFrom)
	}

	byCode := map[string]*domain.JurisdictionRule{}
	for _, r := range pack.Rules {
		if _, dup := byCode[r.RuleCode]; dup {
			t.Errorf("rule_code %q appears twice — the pack must resolve to one winner per code", r.RuleCode)
		}
		byCode[r.RuleCode] = r
	}

	if len(byCode) != 2 {
		t.Fatalf("expected 2 resolved TAX rules (FILING_FREQ, FEDERAL_ONLY), got %d: %v", len(byCode), keysOf(byCode))
	}
	// The state's override must win over the country's.
	if got := byCode["FILING_FREQ"]; got == nil || got.JurisdictionID != state.JurisdictionID {
		t.Errorf("FILING_FREQ resolved to the wrong jurisdiction — the nearest one must win")
	}
	// The federal rule with no state override must still be inherited.
	if got := byCode["FEDERAL_ONLY"]; got == nil || got.JurisdictionID != country.JurisdictionID {
		t.Error("FEDERAL_ONLY should be inherited from the country")
	}
	if _, present := byCode["STATE_DRAFT"]; present {
		t.Error("a DRAFT rule must never enter a runtime rule pack")
	}
	if _, present := byCode["STATE_RETIRED"]; present {
		t.Error("a RETIRED rule must never enter a runtime rule pack")
	}
	if _, present := byCode["STATE_PAYROLL"]; present {
		t.Error("domain=TAX must exclude PAYROLL rules")
	}

	// Unfiltered, the PAYROLL rule joins the pack.
	all, err := s.FindRulePack(ctx, state.JurisdictionID, "", at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all.Rules) != 3 {
		t.Errorf("expected 3 rules with no domain filter, got %d", len(all.Rules))
	}
}

// TestPgStore_FindRulePack_InactiveJurisdictionFailsClosed — a pack is a
// runtime artifact, and an end-dated jurisdiction has no runtime rules.
func TestPgStore_FindRulePack_InactiveJurisdictionFailsClosed(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	if _, _, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "admin-1"); err != nil {
		t.Fatalf("failed to deactivate: %v", err)
	}

	if _, err := s.FindRulePack(ctx, j.JurisdictionID, "", time.Now().UTC()); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound for a deactivated jurisdiction, got %v", err)
	}
	if _, err := s.FindRulePack(ctx, uuid.New().String(), "", time.Now().UTC()); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound for an unknown jurisdiction, got %v", err)
	}
}

// TestPgStore_FindRulePack_HistoricalReplayAfterDeactivation — a pack is a
// runtime artifact, but "historical actions must always be explainable against
// the rule set active at the time of execution" (03-microservices.md §8.2). A
// deactivated jurisdiction therefore fails closed only for `at` outside its
// effective window; an `at` from before its retirement still replays the pack.
func TestPgStore_FindRulePack_HistoricalReplayAfterDeactivation(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	before := time.Now().UTC().Add(-time.Hour)
	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "GB_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        before.Add(-time.Hour),
		RulePayload:          []byte(`{"filing_frequency": "QUARTERLY"}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	pre, err := s.FindRulePack(ctx, j.JurisdictionID, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("expected a live pack before deactivation, got %v", err)
	}
	if len(pre.Rules) != 1 {
		t.Fatalf("expected 1 rule before deactivation, got %d", len(pre.Rules))
	}

	if _, _, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "admin-1"); err != nil {
		t.Fatalf("failed to deactivate: %v", err)
	}

	if _, err := s.FindRulePack(ctx, j.JurisdictionID, "", time.Now().UTC().Add(time.Minute)); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound for `at` after the end of the window, got %v", err)
	}
	hist, err := s.FindRulePack(ctx, j.JurisdictionID, "", before)
	if err != nil {
		t.Fatalf("a historical pack after deactivation must still answer, got %v", err)
	}
	if len(hist.Rules) != 1 || hist.Rules[0].JurisdictionRuleID != r.JurisdictionRuleID {
		t.Errorf("historical pack got %d rules; want the single rule that was in force", len(hist.Rules))
	}
}

// TestPgStore_FindRulePack_AncestorRulesEndWithTheirJurisdiction — an inherited
// rule is only resolvable while the ancestor that owns it is itself effective,
// even when the rule row carries no end date. A retired ancestor must stop
// contributing to a later pack while still contributing to an earlier one.
func TestPgStore_FindRulePack_AncestorRulesEndWithTheirJurisdiction(t *testing.T) {
	s, _, ctx := newTestStore(t)
	country := mustCreateJurisdiction(t, s, ctx, "CA", nil)
	state := mustCreateJurisdiction(t, s, ctx, "CA-ON", &country.JurisdictionID)

	before := time.Now().UTC().Add(-time.Hour)
	countryRule, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       country.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "CA_FEDERAL",
		RuleName:             "Federal",
		EffectiveFrom:        before.Add(-2 * time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create country rule: %v", err)
	}
	if _, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID:       state.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "ON_PAYROLL",
		RuleName:             "Payroll",
		EffectiveFrom:        before.Add(-time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("failed to create state rule: %v", err)
	}

	inherited, err := s.FindRulePack(ctx, state.JurisdictionID, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error on live pack: %v", err)
	}
	if len(inherited.Rules) != 2 {
		t.Fatalf("expected 2 inherited rules while both are active, got %d", len(inherited.Rules))
	}

	if _, _, err := s.DeactivateJurisdiction(ctx, country.JurisdictionID, "admin-1"); err != nil {
		t.Fatalf("failed to deactivate country: %v", err)
	}

	onlyState, err := s.FindRulePack(ctx, state.JurisdictionID, "", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error on the state pack after the country retired: %v", err)
	}
	for _, rule := range onlyState.Rules {
		if rule.JurisdictionRuleID == countryRule.JurisdictionRuleID {
			t.Fatal("a retired country's open-ended rule must not resolve into a later state pack")
		}
	}
	if len(onlyState.Rules) != 1 {
		t.Fatalf("expected only the state rule to survive, got %d", len(onlyState.Rules))
	}

	historic, err := s.FindRulePack(ctx, state.JurisdictionID, "", before)
	if err != nil {
		t.Fatalf("a historical state pack must still include the country rule, got %v", err)
	}
	var found bool
	for _, rule := range historic.Rules {
		found = found || rule.JurisdictionRuleID == countryRule.JurisdictionRuleID
	}
	if !found {
		t.Error("historical state pack must retain the retired country's rule")
	}
}

// TestPgStore_AppendOnlyEnforced — drift history and rule status history are
// append-only (000009 + 000017). Direct UPDATE/DELETE on either table must be
// denied in the database, not merely discouraged by the store API.
func TestPgStore_AppendOnlyEnforced(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "NL", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "NL_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{"filing_frequency": "MONTHLY"}`),
		RuleStatus:           "DRAFT",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}
	if _, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "actor-1",
	}); err != nil {
		t.Fatalf("failed to transition DRAFT -> ACTIVE: %v", err)
	}
	if _, event, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "DRIFTED",
		RecordedByPrincipalID: "admin-1",
	}); err != nil || event == nil {
		t.Fatalf("failed to record drift: %v", err)
	}

	assertAppendOnly := func(table, column string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE "+table+" SET "+column+" = "+column+" WHERE jurisdiction_rule_id = $1", r.JurisdictionRuleID); err == nil {
			t.Errorf("%s: an UPDATE must be denied (append-only)", table)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE jurisdiction_rule_id = $1", r.JurisdictionRuleID); err == nil {
			t.Errorf("%s: a DELETE must be denied (append-only)", table)
		}
	}

	// rule_status_history: UPDATE is legitimate (the record_rule_status_change
	// trigger writes known_to on close-out), so only DELETE is asserted there.
	if _, err := pool.Exec(ctx, "DELETE FROM rule_status_history WHERE jurisdiction_rule_id = $1", r.JurisdictionRuleID); err == nil {
		t.Error("rule_status_history: a DELETE must be denied (append-only)")
	}
	assertAppendOnly("jurisdiction_rule_drift_events", "reason")
}

// TestPgStore_FindRulePack_StatusHistoryGovernsHistoricalPacks — the pack's
// status test is bitemporal: it is the status KNOWN at `at` that decides, not
// the rule's current status. These two rules share the same "today" (one still
// ACTIVE, one RETIRED at runtime) but differ in what the platform knew earlier.
//
// The known timeline is seeded directly because a runtime DRAFT→ACTIVE or
// ACTIVE→RETIRED transition collapses into the same timestamp as creation —
// the only way to pin a historical `at` deterministically is to write the
// rows the trigger would have written.
func TestPgStore_FindRulePack_StatusHistoryGovernsHistoricalPacks(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	now := time.Now().UTC()
	from := now.Add(-6 * time.Hour)

	newRule := func(jID, code, status string) *domain.JurisdictionRule {
		t.Helper()
		r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
			JurisdictionRuleID:   uuid.New().String(),
			JurisdictionID:       jID,
			RuleDomain:           "TAX",
			RuleCode:             code,
			RuleName:             code,
			EffectiveFrom:        from,
			RulePayload:          []byte(`{"rate":"0.2"}`),
			RuleStatus:           status,
			CreatedByPrincipalID: "admin-1",
		})
		if err != nil {
			t.Fatalf("failed to create rule %s: %v", code, err)
		}
		return r
	}

	seedHistory := func(ruleID, status string, knownFrom, knownTo time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO rule_status_history
			 (jurisdiction_rule_id, rule_status, effective_from, known_from, known_to,
			  changed_by_principal_id, schema_version)
			 VALUES ($1, $2, $3, $4, $5, 'admin-1', '1.0')`,
			ruleID, status, from, knownFrom, knownTo); err != nil {
			t.Fatalf("failed to seed history row for %s: %v", ruleID, err)
		}
	}

	find := func(jID string, at time.Time) []*domain.JurisdictionRule {
		t.Helper()
		pack, err := s.FindRulePack(ctx, jID, "", at)
		if err != nil {
			t.Fatalf("FindRulePack at %s: %v", at, err)
		}
		return pack.Rules
	}
	has := func(rules []*domain.JurisdictionRule, id string) bool {
		for _, rule := range rules {
			if rule.JurisdictionRuleID == id {
				return true
			}
		}
		return false
	}

	// draftNow — currently ACTIVE, but the platform knew it as DRAFT until
	// now-1h. At now-2h it must NOT govern (audit: "a rule activated after its
	// effective_from appears effective for dates when it was still DRAFT").
	j := mustCreateJurisdiction(t, s, ctx, "JP", nil)
	draftNow := newRule(j.JurisdictionID, "JP_CTT", "ACTIVE")
	seedHistory(draftNow.JurisdictionRuleID, "DRAFT", now.Add(-3*time.Hour), now.Add(-time.Hour))

	for _, rule := range find(j.JurisdictionID, now.Add(-2*time.Hour)) {
		if rule.JurisdictionRuleID == draftNow.JurisdictionRuleID {
			t.Fatal("a rule the platform knew as DRAFT at `at` must not govern the historical pack")
		}
	}
	if !has(find(j.JurisdictionID, now), draftNow.JurisdictionRuleID) {
		t.Error("the same rule must govern today, where the ACTIVE row is in force")
	}

	// retiredNow — retired at runtime (current status RETIRED), but ACTIVE
	// until now-1h in the history. It must STILL govern a pack for now-2h
	// (audit: "a RETIRED rule vanishes from historical packs, even for dates
	// when it governed").
	j2 := mustCreateJurisdiction(t, s, ctx, "IE", nil)
	retiredNow := newRule(j2.JurisdictionID, "IE_VAT", "ACTIVE")
	seedHistory(retiredNow.JurisdictionRuleID, "ACTIVE", now.Add(-3*time.Hour), now.Add(-time.Hour))
	if _, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        retiredNow.JurisdictionRuleID,
		NewStatus:     "RETIRED",
		AllowedPriors: []string{"ACTIVE"},
		EndDate:       true,
		ActorID:       "admin-1",
	}); err != nil {
		t.Fatalf("failed to retire rule: %v", err)
	}

	if !has(find(j2.JurisdictionID, now.Add(-2*time.Hour)), retiredNow.JurisdictionRuleID) {
		t.Error("a since-RETIRED rule must remain in historical packs for the dates it governed")
	}
	if has(find(j2.JurisdictionID, now.Add(30*time.Second)), retiredNow.JurisdictionRuleID) {
		t.Error("a RETIRED rule must not govern a later pack")
	}
}

func keysOf(m map[string]*domain.JurisdictionRule) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
