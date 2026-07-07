package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	migPath := filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000001_initial_schema.up.sql")
	migSQL, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", migPath, err)
	}

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS governance_decisions CASCADE;`)
	if _, err := pool.Exec(ctx, string(migSQL)); err != nil {
		t.Fatalf("failed to execute migration: %v", err)
	}

	return pool
}

func sampleDecision(id string) domain.GovernanceDecision {
	return domain.GovernanceDecision{
		DecisionID:    id,
		TenantID:      "tenant-1",
		LegalEntityID: "entity-1",
		ActorID:       "actor-1",
		ActionType:    "PAYROLL_RELEASE",
		Outcome:       "DENIED",
		RuleBasis:     "policy-v3-sod",
		CorrelationID: "corr-1",
		DecidedAt:     time.Now().UTC().Truncate(time.Microsecond),
	}
}

// TestPgStore_Insert_Integration verifies a first insert lands a real row
// with all columns intact.
func TestPgStore_Insert_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	d := sampleDecision("dec-int-001")
	created, err := s.Insert(ctx, d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for first insert")
	}

	got, err := s.FindByID(ctx, "dec-int-001")
	if err != nil {
		t.Fatalf("unexpected error on FindByID: %v", err)
	}
	if got.TenantID != d.TenantID || got.Outcome != d.Outcome || got.RuleBasis != d.RuleBasis {
		t.Errorf("stored row does not match input: got %+v, want %+v", got, d)
	}
}

// TestPgStore_Insert_IdempotentOnDuplicateDecisionID is the critical test:
// posting the same decision_id twice must not create a duplicate row, and
// must not error on the second call.
func TestPgStore_Insert_IdempotentOnDuplicateDecisionID(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	d := sampleDecision("dec-int-dup")

	created1, err := s.Insert(ctx, d)
	if err != nil {
		t.Fatalf("unexpected error on first insert: %v", err)
	}
	if !created1 {
		t.Fatalf("expected created=true on first insert")
	}

	// Second insert with the same decision_id but different payload —
	// must be a silent no-op, not an overwrite (append-only evidence rule).
	dup := d
	dup.Outcome = "GRANTED"
	created2, err := s.Insert(ctx, dup)
	if err != nil {
		t.Fatalf("unexpected error on duplicate insert: %v", err)
	}
	if created2 {
		t.Fatalf("expected created=false on duplicate decision_id, got true")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM governance_decisions WHERE decision_id = $1`, d.DecisionID).Scan(&count); err != nil {
		t.Fatalf("failed to count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for decision_id %q, got %d", d.DecisionID, count)
	}

	// The original outcome must have won — no silent overwrite.
	got, err := s.FindByID(ctx, d.DecisionID)
	if err != nil {
		t.Fatalf("unexpected error on FindByID: %v", err)
	}
	if got.Outcome != "DENIED" {
		t.Errorf("expected original outcome DENIED to be preserved, got %q", got.Outcome)
	}
}

// TestPgStore_FindByID_NotFound verifies an unknown decision_id returns
// domain.ErrDecisionNotFound.
func TestPgStore_FindByID_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.FindByID(ctx, "does-not-exist")
	if err != domain.ErrDecisionNotFound {
		t.Fatalf("expected ErrDecisionNotFound, got %v", err)
	}
}

// seedListFixtures inserts a small, deliberately varied set of decisions
// covering every List filter dimension (actor, entity, action, rule basis,
// and a spread of decided_at timestamps) so filter tests can assert on
// exact membership.
func seedListFixtures(t *testing.T, ctx context.Context, s *store.PgStore) {
	t.Helper()
	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	fixtures := []domain.GovernanceDecision{
		{
			DecisionID: "dec-list-1", TenantID: "tenant-1", LegalEntityID: "entity-A",
			ActorID: "actor-1", ActionType: "PAYROLL_RELEASE", Outcome: "DENIED",
			RuleBasis: "policy-v3-sod", CorrelationID: "corr-1", DecidedAt: base,
		},
		{
			DecisionID: "dec-list-2", TenantID: "tenant-1", LegalEntityID: "entity-B",
			ActorID: "actor-2", ActionType: "PAYROLL_RELEASE", Outcome: "GRANTED",
			RuleBasis: "policy-v3-sod", CorrelationID: "corr-2", DecidedAt: base.AddDate(0, 0, 1),
		},
		{
			DecisionID: "dec-list-3", TenantID: "tenant-1", LegalEntityID: "entity-A",
			ActorID: "actor-1", ActionType: "TAX_FILING", Outcome: "GRANTED",
			RuleBasis: "policy-v9-tax", CorrelationID: "corr-3", DecidedAt: base.AddDate(0, 0, 2),
		},
	}
	for _, d := range fixtures {
		if _, err := s.Insert(ctx, d); err != nil {
			t.Fatalf("failed to seed fixture %q: %v", d.DecisionID, err)
		}
	}
}

// TestPgStore_List_NoFilters_ReturnsAllNewestFirst verifies List with no
// filters returns every row, ordered by decided_at descending.
func TestPgStore_List_NoFilters_ReturnsAllNewestFirst(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].DecisionID != "dec-list-3" || results[2].DecisionID != "dec-list-1" {
		t.Errorf("expected newest-first ordering, got order: %s, %s, %s",
			results[0].DecisionID, results[1].DecisionID, results[2].DecisionID)
	}
}

// TestPgStore_List_FilterByActor verifies the actor filter in isolation.
func TestPgStore_List_FilterByActor(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{ActorID: "actor-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].DecisionID != "dec-list-2" {
		t.Fatalf("expected exactly dec-list-2, got %+v", results)
	}
}

// TestPgStore_List_FilterByEntity verifies the entity (legal_entity_id)
// filter in isolation.
func TestPgStore_List_FilterByEntity(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{LegalEntityID: "entity-A"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for entity-A, got %d", len(results))
	}
}

// TestPgStore_List_FilterByAction verifies the action_type filter in isolation.
func TestPgStore_List_FilterByAction(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{ActionType: "TAX_FILING"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].DecisionID != "dec-list-3" {
		t.Fatalf("expected exactly dec-list-3, got %+v", results)
	}
}

// TestPgStore_List_FilterByRuleBasis verifies the rule_basis filter in isolation.
func TestPgStore_List_FilterByRuleBasis(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{RuleBasis: "policy-v3-sod"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for policy-v3-sod, got %d", len(results))
	}
}

// TestPgStore_List_FilterByTimeRange verifies the from/to time range filter
// in isolation, using a half-open-in-practice inclusive bound.
func TestPgStore_List_FilterByTimeRange(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	results, err := s.List(ctx, store.ListParams{
		From: base.AddDate(0, 0, 1),
		To:   base.AddDate(0, 0, 2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results in range, got %d: %+v", len(results), results)
	}
}

// TestPgStore_List_FiltersCompose verifies multiple filters applied together
// narrow the result set with AND semantics, not OR.
func TestPgStore_List_FiltersCompose(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{
		ActorID:       "actor-1",
		LegalEntityID: "entity-A",
		RuleBasis:     "policy-v9-tax",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].DecisionID != "dec-list-3" {
		t.Fatalf("expected exactly dec-list-3, got %+v", results)
	}
}

// TestPgStore_List_NoMatch_ReturnsEmptyNotError verifies a filter combination
// matching nothing returns an empty slice, not an error.
func TestPgStore_List_NoMatch_ReturnsEmptyNotError(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedListFixtures(t, ctx, s)

	results, err := s.List(ctx, store.ListParams{ActorID: "does-not-exist"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}
