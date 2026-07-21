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

// openTestPool connects to a real Postgres and reapplies the migrations
// from a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set —
// same convention as every other service in this platform.
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
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS intercompany_entries CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency_index.up.sql",
	} {
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
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}

	return pool
}

func newTestEntry(tenantID, sourceJournalID string) *domain.IntercompanyEntry {
	now := time.Now().UTC()
	return &domain.IntercompanyEntry{
		IntercompanyEntryID: uuid.New().String(),
		TenantID:            tenantID,
		SourceLegalEntityID: "le-us",
		TargetLegalEntityID: "le-uk",
		SourceJournalID:     sourceJournalID,
		Amount:              5000,
		CurrencyCode:        "USD",
		MatchStatus:         "UNMATCHED",
		CreatedAt:           now,
		UpdatedAt:           now,
	}
}

// TestPgStore_CreateEntry_RetriedSourceJournalID_IsIdempotent proves the
// idempotency guarantee against a REAL Postgres unique index — this is the
// exact scenario a network-timeout-triggered client retry produces, and it
// must resolve to the original entry, never a duplicate.
func TestPgStore_CreateEntry_RetriedSourceJournalID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	sourceJournalID := uuid.New().String()
	entry1 := newTestEntry(tenantID, sourceJournalID)
	created1, err := s.CreateEntry(ctx, entry1)
	if err != nil {
		t.Fatalf("first CreateEntry failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	// Simulate a client retry: a fresh entry (new IntercompanyEntryID, as a
	// real client would generate) but the SAME source_journal_id.
	entry2 := newTestEntry(tenantID, sourceJournalID)
	created2, err := s.CreateEntry(ctx, entry2)
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
	s := store.New(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	entryA := newTestEntry(tenantA, uuid.New().String())
	if _, err := s.CreateEntry(ctxA, entryA); err != nil {
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
