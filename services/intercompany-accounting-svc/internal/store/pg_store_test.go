package store_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

// migrations is every up migration, in order. openTestPool applies all of
// them; a migration test applies a prefix, seeds rows the way the older
// schema allowed, then applies the rest.
var migrations = []string{
	"000001_initial_schema.up.sql",
	"000002_add_idempotency_index.up.sql",
	"000003_add_acc11_lifecycle.up.sql",
	"000004_add_eventing_outbox.up.sql",
	"000005_pair_counterparty_period_dimensions.up.sql",
	"000006_pair_closed_state_transitions.up.sql",
	"000007_match_attempts.up.sql",
	"000008_source_journal_checks.up.sql",
	"000009_command_idempotency.up.sql",
}

// resetTables is every table the migrations create, children first.
// DROP ... CASCADE on a parent drops only the child's foreign key, not the
// child table, so each is dropped by name.
var resetTables = []string{
	"command_idempotency",
	"intercompany_match_attempts",
	"intercompany_pair_transitions",
	"intercompany_entries",
	"eventing_outbox",
}

// openTestPool connects to a real Postgres and reapplies the migrations
// from a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set —
// same convention as every other service in this platform.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openEmptyPool(t)
	applyMigrations(t, pool, migrations...)
	return pool
}

// openEmptyPool connects and drops every table the migrations create, so
// the caller decides how far to migrate.
func openEmptyPool(t *testing.T) *pgxpool.Pool {
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

	for _, table := range resetTables {
		if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS `+table+` CASCADE`); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	return pool
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)
	for _, migration := range names {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", migration, err)
		}
		// 000001 has a leading UTF-8 BOM (bytes EF BB BF) — psql -f (the real
		// deployment path) strips it automatically, but pgx sends the raw
		// bytes as a wire-protocol query string, which Postgres's parser
		// rejects. Strip it here so this test exercises the same SQL psql
		// would actually apply.
		sql = bytes.TrimPrefix(sql, []byte("\xef\xbb\xbf"))
		if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}
}

func newTestEntry(tenantID, sourceJournalID string) *domain.IntercompanyEntry {
	now := time.Now().UTC()
	return &domain.IntercompanyEntry{
		IntercompanyEntryID: uuid.New().String(),
		TenantID:            tenantID,
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		SourceJournalID:     sourceJournalID,
		Amount:              "5000",
		CounterpartyAmount:  "5000", CounterpartyCurrencyCode: "USD",
		CurrencyCode: "USD",
		MatchStatus:  "UNMATCHED",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// TestPgStore_CreateEntry_RetriedSourceJournalID_IsIdempotent proves the
// idempotency guarantee against a REAL Postgres unique index — this is the
// exact scenario a network-timeout-triggered client retry produces, and it
// must resolve to the original entry, never a duplicate.
func TestPgStore_CreateEntry_RetriedSourceJournalID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	sourceJournalID := uuid.New().String()
	entry1 := newTestEntry(tenantID, sourceJournalID)
	created1, err := s.CreateEntry(ctx, entry1, "actor-1", "corr-1")
	if err != nil {
		t.Fatalf("first CreateEntry failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	// Simulate a client retry: a fresh entry (new IntercompanyEntryID, as a
	// real client would generate) but the SAME source_journal_id.
	entry2 := newTestEntry(tenantID, sourceJournalID)
	created2, err := s.CreateEntry(ctx, entry2, "actor-1", "corr-2")
	if err != nil {
		t.Fatalf("retried CreateEntry failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-entry bug if it's true")
	}
	if entry2.IntercompanyEntryID != entry1.IntercompanyEntryID {
		t.Fatalf("retried call resolved to a different intercompany_entry_id (%s) than the original (%s)", entry2.IntercompanyEntryID, entry1.IntercompanyEntryID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM intercompany_entries WHERE tenant_id = $1 AND source_journal_id = $2`,
		tenantID, sourceJournalID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("DUPLICATE ENTRY: expected exactly 1 intercompany_entries row for this source_journal_id, got %d", count)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	entryA := newTestEntry(tenantA, uuid.New().String())
	if _, err := s.CreateEntry(ctxA, entryA, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry (tenant A) failed: %v", err)
	}

	// Query tenant A's entry while scoped to tenant B's RLS context — RLS
	// must hide it entirely, proving tenant isolation actually holds, not
	// just that the column exists.
	_, err := s.GetEntry(ctxB, entryA.IntercompanyEntryID)
	if err == nil {
		t.Fatal("RLS failure: tenant B's session was able to read tenant A's intercompany entry")
	}
}

// testAttempt is the minimal evidence UpdateMatch requires for the outcome
// entry already carries, for tests whose subject is not the attempt itself.
func testAttempt(entry *domain.IntercompanyEntry) *domain.MatchAttempt {
	a := &domain.MatchAttempt{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		Outcome:             entry.MatchStatus,
		Source: domain.MatchAttemptSide{
			JournalID: &entry.SourceJournalID, ExpectedAmount: entry.Amount, ExpectedCurrency: entry.CurrencyCode,
		},
		Target: domain.MatchAttemptSide{
			JournalID: entry.TargetJournalID, ExpectedAmount: entry.CounterpartyAmount, ExpectedCurrency: entry.CounterpartyCurrencyCode,
		},
		ExaminedJournalIDs: []string{},
		Tolerance:          "0.001",
		AttemptedBy:        "test",
	}
	if entry.MatchStatus == domain.MatchStatusMismatch {
		rule := domain.MatchRuleAmountMismatch
		a.FailedRule, a.Message = &rule, entry.MismatchReason
	}
	return a
}
