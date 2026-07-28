package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS journal_lines, journal_headers CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency_index.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}

	return pool
}

func TestPgStore_CreateJournal_And_GetJournal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	h := &domain.JournalHeader{
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "test journal",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-1",
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}

	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}

	got, gotLines, err := s.GetJournal(ctx, h.JournalID)
	if err != nil {
		t.Fatalf("GetJournal failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected journal to be found")
	}
	if got.Status != domain.JournalStatusPending {
		t.Fatalf("expected status PENDING, got %s", got.Status)
	}
	if len(gotLines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(gotLines))
	}
	if gotLines[0].LineNumber != 1 || gotLines[1].LineNumber != 2 {
		t.Fatalf("expected line numbers assigned in order, got %d, %d", gotLines[0].LineNumber, gotLines[1].LineNumber)
	}
}

// TestPgStore_CreateJournal_RetriedCorrelationID_IsIdempotent proves the
// idempotency guarantee against a REAL Postgres unique index, not a stub —
// this is exactly the scenario a network-timeout-triggered client retry
// produces, and it must resolve to the original journal, never a duplicate.
func TestPgStore_CreateJournal_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	newHeader := func() *domain.JournalHeader {
		return &domain.JournalHeader{
			JournalID:            uuid.New().String(),
			TenantID:             tenantID,
			LegalEntityID:        legalEntityID,
			FiscalPeriod:         "2026-07",
			Status:               domain.JournalStatusPending,
			Description:          "retried journal",
			CreatedByPrincipalID: "test-admin",
			CorrelationID:        "corr-retry-1",
		}
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}

	h1 := newHeader()
	resultLines1, created1, err := s.CreateJournal(ctx, h1, lines)
	if err != nil {
		t.Fatalf("first CreateJournal failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}
	if len(resultLines1) != 2 {
		t.Fatalf("expected 2 lines on first call, got %d", len(resultLines1))
	}

	// Simulate a client retry: a fresh header (new JournalID, as a real
	// client would generate) but the SAME correlation_id.
	h2 := newHeader()
	resultLines2, created2, err := s.CreateJournal(ctx, h2, lines)
	if err != nil {
		t.Fatalf("retried CreateJournal failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-posting bug if it's true")
	}
	if h2.JournalID != h1.JournalID {
		t.Fatalf("retried call resolved to a different journal_id (%s) than the original (%s)", h2.JournalID, h1.JournalID)
	}
	if len(resultLines2) != 2 {
		t.Fatalf("expected the original journal's 2 lines to be returned on replay, got %d", len(resultLines2))
	}

	// Confirm only ONE journal actually exists in the database for this
	// correlation_id — the real assertion this test exists to make.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2`,
		tenantID, "corr-retry-1").Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("DUPLICATE POSTING: expected exactly 1 journal_headers row for this correlation_id, got %d", count)
	}
}

func TestPgStore_SumLines(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	h := &domain.JournalHeader{
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "sum test",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-2",
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 60},
		{AccountCode: "1001", DebitAmount: 40},
		{AccountCode: "4000", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}

	debitTotal, creditTotal, err := s.SumLines(ctx, tenantID, h.JournalID)
	if err != nil {
		t.Fatalf("SumLines failed: %v", err)
	}
	if debitTotal != 100 || creditTotal != 100 {
		t.Fatalf("expected debit=100 credit=100, got debit=%v credit=%v", debitTotal, creditTotal)
	}
}

func TestPgStore_TransitionJournal_WrongFromStatus_Rejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	h := &domain.JournalHeader{
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "transition test",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-3",
	}
	if _, _, err := s.CreateJournal(ctx, h, []domain.JournalLine{{AccountCode: "1000", DebitAmount: 1}}); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}

	// Journal is PENDING — attempting VALIDATED -> FINALIZED (wrong fromStatus)
	// must be rejected as a no-op (0 rows affected), never silently succeed.
	err := s.TransitionJournal(ctx, tenantID, h.JournalID, domain.JournalStatusValidated, domain.JournalStatusFinalized, "test-admin")
	if err == nil {
		t.Fatal("expected an error transitioning from the wrong fromStatus, got nil")
	}

	got, _, _ := s.GetJournal(ctx, h.JournalID)
	if got.Status != domain.JournalStatusPending {
		t.Fatalf("journal status must remain unchanged after a rejected transition, got %s", got.Status)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	hA := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantA, LegalEntityID: uuid.New().String(),
		FiscalPeriod: "2026-07", Status: domain.JournalStatusPending,
		Description: "tenant A journal", CreatedByPrincipalID: "admin-a", CorrelationID: "corr-a",
	}
	if _, _, err := s.CreateJournal(ctxA, hA, []domain.JournalLine{{AccountCode: "1000", DebitAmount: 1}}); err != nil {
		t.Fatalf("CreateJournal (tenant A) failed: %v", err)
	}

	// Query tenant A's journal while scoped to tenant B's RLS context — RLS
	// must hide it entirely, proving tenant isolation actually holds, not
	// just that the column exists.
	got, _, err := s.GetJournal(ctxB, hA.JournalID)
	if err != nil {
		t.Fatalf("GetJournal under tenant B's context returned an error rather than a clean not-found: %v", err)
	}
	if got != nil {
		t.Fatal("RLS failure: tenant B's session was able to read tenant A's journal")
	}
}
