package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS journal_lines, journal_headers CASCADE;`)

	// Every *.up.sql, sorted, rather than a list written out here.
	//
	// The list stopped at 000003, so this suite has been running against a
	// schema no deployment has: 000004 is the one that FORCEs row-level
	// security, which is the migration a store test most needs applied — the
	// tests could not have caught the policy being wrong because the policy was
	// never in force. Globbing means the next migration lands here too.
	migrationDir := filepath.Join(base, "../../deployments/migrations")
	migrations, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to glob migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatalf("no *.up.sql migrations found under %s", migrationDir)
	}
	sort.Strings(migrations)

	for _, migration := range migrations {
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(migration), err)
		}
	}

	return pool
}

// scoped runs a verification query with app.tenant_id installed, exactly as the
// store installs it for every statement it makes.
//
// The assertions below deliberately query the tables directly rather than
// through the store: a write must not be verified by the code that performed
// it. But a raw query carries no tenant scope, and once the row-level security
// policy actually applies — which it does the moment the service connects as
// something other than a superuser — an unscoped SELECT returns NO rows. The
// assertion then reports "this row was never written" about a row that was,
// which is a false alarm on the most alarming subject in the ledger.
//
// The scope is transaction-local, so nothing leaks back onto the pooled
// connection. That matters: a custom GUC set non-locally survives on the
// connection, and a stale tenant there would silently scope a later query to
// the wrong tenant.
func scoped(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("verification tx begin failed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("installing the verification tenant scope failed: %v", err)
	}
	fn(tx)
}

// requireThrowawayDatabase refuses to run against anything not recognisably
// disposable. This suite DROPs journal_headers and journal_lines on setup, and
// the general ledger is the one table in this estate where "we can re-seed it"
// is not a consolation. Only the database NAME vouches for it — a password
// that happens to contain "test" must not.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs journal_headers and journal_lines. Use "+
			"general_ledger_test (or CI's testdb), not general_ledger.", dbName)
	}
}

func TestPgStore_CreateJournal_And_GetJournal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	h := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
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
			JournalType:          domain.JournalTypeStandard,
			TransactionDate:      domain.NewDate(2026, 7, 15),
			PostingDate:          domain.NewDate(2026, 7, 15),
			CurrencyCode:         "GBP",
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
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2`,
			tenantID, "corr-retry-1").Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
	})
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
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "sum test",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-2",
	}
	// Amounts chosen so the sums are unrepresentable in binary floating point:
	// 0.10 + 0.20 == 0.30 is false in float64, and this journal balances only
	// if the totals are compared as exact minor units. Sums are returned in
	// cents for that reason.
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 60.10},
		{AccountCode: "1001", DebitAmount: 40.20},
		{AccountCode: "4000", CreditAmount: 100.30},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}

	debitTotal, creditTotal, err := s.SumLines(ctx, tenantID, h.JournalID)
	if err != nil {
		t.Fatalf("SumLines failed: %v", err)
	}
	if debitTotal != 10030 || creditTotal != 10030 {
		t.Fatalf("expected debit=10030c credit=10030c, got debit=%d credit=%d", debitTotal, creditTotal)
	}
	if debitTotal != creditTotal {
		t.Fatal("a journal that balances in decimal must balance here — this is the float-equality trap")
	}
}

func TestPgStore_TransitionJournal_WrongFromStatus_Rejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	h := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
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
		JournalType:     domain.JournalTypeStandard,
		TransactionDate: domain.NewDate(2026, 7, 15),
		PostingDate:     domain.NewDate(2026, 7, 15),
		CurrencyCode:    "GBP",
		JournalID:       uuid.New().String(), TenantID: tenantA, LegalEntityID: uuid.New().String(),
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

// ── reversal ─────────────────────────────────────────────────────────────────

// newFinalizedJournal creates a journal and walks it PENDING -> VALIDATED ->
// FINALIZED, which is the only state a reversal may start from.
func newFinalizedJournal(t *testing.T, s *store.PgStore, ctx context.Context, tenantID string) *domain.JournalHeader {
	t.Helper()
	h := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "original posting",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-" + uuid.New().String(),
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 250.75},
		{AccountCode: "4000", CreditAmount: 250.75},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}
	if err := s.TransitionJournal(ctx, tenantID, h.JournalID,
		domain.JournalStatusPending, domain.JournalStatusValidated, "test-admin"); err != nil {
		t.Fatalf("transition to VALIDATED failed: %v", err)
	}
	if err := s.TransitionJournal(ctx, tenantID, h.JournalID,
		domain.JournalStatusValidated, domain.JournalStatusFinalized, "test-admin"); err != nil {
		t.Fatalf("transition to FINALIZED failed: %v", err)
	}
	return h
}

func newReversingHeader(tenantID, originalID, correlationID string, original *domain.JournalHeader) *domain.JournalHeader {
	principal := "reversing-admin"
	return &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        original.LegalEntityID,
		FiscalPeriod:         original.FiscalPeriod,
		Status:               domain.JournalStatusFinalized,
		ReversalOfJournalID:  &originalID,
		Description:          "Reversal of " + originalID,
		CreatedByPrincipalID: principal,
		PostedByPrincipalID:  &principal,
		CorrelationID:        correlationID,
	}
}

// TestPgStore_ReverseJournal_PersistsReversalLink is the regression test for
// the defect that mattered most here: the INSERT named eleven columns and
// omitted reversal_of_journal_id and the posted_* pair, so the link a reversal
// exists to record was returned to the caller and never written down. Every
// assertion below reads the DATABASE, not the struct the store just mutated —
// asserting on the struct is what let this survive.
func TestPgStore_ReverseJournal_PersistsReversalLink(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	original := newFinalizedJournal(t, s, ctx, tenantID)
	reversing := newReversingHeader(tenantID, original.JournalID, "corr-rev-"+uuid.New().String(), original)
	reversingLines := []domain.JournalLine{
		{AccountCode: "1000", CreditAmount: 250.75},
		{AccountCode: "4000", DebitAmount: 250.75},
	}

	_, created, err := s.ReverseJournal(ctx, tenantID, original.JournalID, reversing, reversingLines, "reversing-admin")
	if err != nil {
		t.Fatalf("ReverseJournal failed: %v", err)
	}
	if !created {
		t.Fatal("expected the reversal to be created")
	}

	var (
		reversalOf *string
		status     string
		postedBy   *string
		postedAt   *time.Time
	)
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `
			SELECT reversal_of_journal_id::text, status, posted_by_principal_id, posted_at
			FROM journal_headers WHERE journal_id = $1`, reversing.JournalID).
			Scan(&reversalOf, &status, &postedBy, &postedAt); err != nil {
			t.Fatalf("reading back the reversing journal failed: %v", err)
		}
	})

	if reversalOf == nil || *reversalOf != original.JournalID {
		t.Fatalf("STORED reversing journal has reversal_of_journal_id = %v, expected %s — the link was dropped on insert",
			reversalOf, original.JournalID)
	}
	if status != string(domain.JournalStatusFinalized) {
		t.Fatalf("expected the reversing journal to be stored FINALIZED, got %s", status)
	}
	if postedBy == nil || *postedBy != "reversing-admin" {
		t.Fatalf("expected posted_by_principal_id to be stored, got %v", postedBy)
	}
	if postedAt == nil {
		t.Fatal("a FINALIZED journal with a null posted_at: the response claimed a posting time the table does not have")
	}

	// And the original must now be REVERSED, with its actor recorded.
	var originalStatus string
	var reversedBy *string
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `
			SELECT status, reversed_by_principal_id FROM journal_headers WHERE journal_id = $1`,
			original.JournalID).Scan(&originalStatus, &reversedBy); err != nil {
			t.Fatalf("reading back the original failed: %v", err)
		}
	})
	if originalStatus != string(domain.JournalStatusReversed) {
		t.Fatalf("expected the original to be REVERSED, got %s", originalStatus)
	}
	if reversedBy == nil || *reversedBy != "reversing-admin" {
		t.Fatalf("expected reversed_by_principal_id to be stored, got %v", reversedBy)
	}

	// The original's own lines are never edited — the only sanctioned
	// correction is the new journal.
	_, originalLines, err := s.GetJournal(ctx, original.JournalID)
	if err != nil {
		t.Fatalf("GetJournal(original) failed: %v", err)
	}
	if len(originalLines) != 2 || originalLines[0].DebitAmount != 250.75 {
		t.Fatalf("the original journal's lines were mutated by a reversal: %+v", originalLines)
	}
}

// TestPgStore_ReverseJournal_NotFinalized_RollsBackBothHalves is the
// double-counting regression: as two separate calls, a reversing journal was
// posted FINALIZED and then the original's transition could fail, leaving the
// books holding a posting AND its inverse as live entries.
func TestPgStore_ReverseJournal_NotFinalized_RollsBackBothHalves(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	// PENDING, never posted — so the REVERSED transition cannot apply.
	h := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "never posted",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-" + uuid.New().String(),
	}
	if _, _, err := s.CreateJournal(ctx, h, []domain.JournalLine{{AccountCode: "1000", DebitAmount: 10}}); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}

	reversing := newReversingHeader(tenantID, h.JournalID, "corr-rev-"+uuid.New().String(), h)
	_, created, err := s.ReverseJournal(ctx, tenantID, h.JournalID, reversing,
		[]domain.JournalLine{{AccountCode: "1000", CreditAmount: 10}}, "reversing-admin")

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition reversing a PENDING journal, got %v", err)
	}
	if created {
		t.Fatal("expected created=false")
	}

	// The whole point: the reversing journal must not exist.
	var count int
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM journal_headers WHERE journal_id = $1`,
			reversing.JournalID).Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
	})
	if count != 0 {
		t.Fatal("ORPHAN REVERSAL: a refused reversal left a FINALIZED reversing journal in the ledger, " +
			"which is a double-counted book no later request reconciles")
	}
	var lineCount int
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM journal_lines WHERE journal_id = $1`,
			reversing.JournalID).Scan(&lineCount); err != nil {
			t.Fatalf("line count query failed: %v", err)
		}
	})
	if lineCount != 0 {
		t.Fatalf("expected the reversing journal's lines to roll back too, found %d", lineCount)
	}
}

// ── idempotency ──────────────────────────────────────────────────────────────

// TestPgStore_CreateJournal_Retry_ResolvesFullStoredHeader covers the half of
// the conflict path that used to be left behind: only journal_id and created_at
// were re-read, so a retry answered with the STORED id wearing the CALLER's
// unstored status and description — a journal that reads PENDING to its author
// while the ledger has it FINALIZED.
func TestPgStore_CreateJournal_Retry_ResolvesFullStoredHeader(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	correlationID := "corr-" + uuid.New().String()

	first := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "the journal that was actually stored",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        correlationID,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 5},
		{AccountCode: "4000", CreditAmount: 5},
	}
	if _, _, err := s.CreateJournal(ctx, first, lines); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}
	if err := s.TransitionJournal(ctx, tenantID, first.JournalID,
		domain.JournalStatusPending, domain.JournalStatusValidated, "test-admin"); err != nil {
		t.Fatalf("transition failed: %v", err)
	}

	// The retry arrives after the journal has moved on, and carries different
	// content — a client that re-sent an edited body under the same key.
	retry := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-08",
		Status:               domain.JournalStatusPending,
		Description:          "a description that was never stored",
		CreatedByPrincipalID: "someone-else",
		CorrelationID:        correlationID,
	}
	retryLines, created, err := s.CreateJournal(ctx, retry, []domain.JournalLine{{AccountCode: "9999", DebitAmount: 999}})
	if err != nil {
		t.Fatalf("retried CreateJournal failed: %v", err)
	}
	if created {
		t.Fatal("expected created=false on a reused correlation_id")
	}

	if retry.JournalID != first.JournalID {
		t.Fatalf("retry resolved to journal %s, expected the stored %s", retry.JournalID, first.JournalID)
	}
	if retry.Status != domain.JournalStatusValidated {
		t.Fatalf("retry reported status %s; the stored journal is VALIDATED — the reply described a journal that does not exist",
			retry.Status)
	}
	if retry.Description != "the journal that was actually stored" {
		t.Fatalf("retry echoed the caller's unstored description %q", retry.Description)
	}
	if retry.FiscalPeriod != "2026-07" {
		t.Fatalf("retry echoed the caller's unstored fiscal period %q", retry.FiscalPeriod)
	}
	if len(retryLines) != 2 || retryLines[0].AccountCode != "1000" {
		t.Fatalf("retry returned the caller's unstored lines: %+v", retryLines)
	}

	var count int
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2`,
			tenantID, correlationID).Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
	})
	if count != 1 {
		t.Fatalf("DUPLICATE POSTING: expected 1 journal for this correlation_id, got %d", count)
	}
}

func TestPgStore_GetJournalByCorrelationID(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	otherTenant := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	correlationID := "corr-" + uuid.New().String()

	h := &domain.JournalHeader{
		JournalType:          domain.JournalTypeStandard,
		TransactionDate:      domain.NewDate(2026, 7, 15),
		PostingDate:          domain.NewDate(2026, 7, 15),
		CurrencyCode:         "GBP",
		JournalID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		FiscalPeriod:         "2026-07",
		Status:               domain.JournalStatusPending,
		Description:          "lookup by key",
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        correlationID,
	}
	if _, _, err := s.CreateJournal(ctx, h, []domain.JournalLine{{AccountCode: "1000", DebitAmount: 7}}); err != nil {
		t.Fatalf("CreateJournal failed: %v", err)
	}

	got, lines, err := s.GetJournalByCorrelationID(ctx, tenantID, correlationID)
	if err != nil {
		t.Fatalf("GetJournalByCorrelationID failed: %v", err)
	}
	if got == nil || got.JournalID != h.JournalID {
		t.Fatalf("expected the journal created with that key, got %+v", got)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	// Another tenant's session must not resolve this key.
	foreign, _, err := s.GetJournalByCorrelationID(svcmiddleware.WithTenant(context.Background(), otherTenant), otherTenant, correlationID)
	if err != nil {
		t.Fatalf("cross-tenant lookup errored rather than answering absent: %v", err)
	}
	if foreign != nil {
		t.Fatal("a correlation_id resolved across a tenant boundary")
	}

	// An unused key is absent, not an error.
	missing, _, err := s.GetJournalByCorrelationID(ctx, tenantID, "corr-never-used")
	if err != nil || missing != nil {
		t.Fatalf("expected (nil, nil) for an unused key, got %+v / %v", missing, err)
	}
}

// ── malformed identifiers ────────────────────────────────────────────────────

// TestPgStore_MalformedUUID_IsAbsentNotAnOutage covers the shared defect found
// across this estate: journal_id is a uuid column, so a mistyped id dies inside
// the driver as SQLSTATE 22P02 and used to surface as 503 store_unavailable —
// a typo in a URL reported to an operator as infrastructure being down.
func TestPgStore_MalformedUUID_IsAbsentNotAnOutage(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	got, lines, err := s.GetJournal(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("GetJournal with a malformed id must answer absent, got error: %v", err)
	}
	if got != nil || lines != nil {
		t.Fatal("a malformed id must not resolve to a journal")
	}

	// A write answers "that transition did not happen" — never
	// store_unavailable, and never success.
	err = s.TransitionJournal(ctx, tenantID, "not-a-uuid",
		domain.JournalStatusPending, domain.JournalStatusValidated, "test-admin")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for a malformed journal_id, got %v", err)
	}
}

// ── list bounds ──────────────────────────────────────────────────────────────

func TestPgStore_ListJournals_RespectsLimit(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	for i := 0; i < 5; i++ {
		h := &domain.JournalHeader{
			JournalType:          domain.JournalTypeStandard,
			TransactionDate:      domain.NewDate(2026, 7, 15),
			PostingDate:          domain.NewDate(2026, 7, 15),
			CurrencyCode:         "GBP",
			JournalID:            uuid.New().String(),
			TenantID:             tenantID,
			LegalEntityID:        uuid.New().String(),
			FiscalPeriod:         "2026-07",
			Status:               domain.JournalStatusPending,
			Description:          "page test",
			CreatedByPrincipalID: "test-admin",
			CorrelationID:        "corr-" + uuid.New().String(),
		}
		if _, _, err := s.CreateJournal(ctx, h, []domain.JournalLine{{AccountCode: "1000", DebitAmount: 1}}); err != nil {
			t.Fatalf("CreateJournal failed: %v", err)
		}
	}

	page, err := s.ListJournals(ctx, domain.ListJournalsFilter{TenantID: tenantID, Limit: 2})
	if err != nil {
		t.Fatalf("ListJournals failed: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("expected the limit to bound the page at 2, got %d", len(page))
	}

	all, err := s.ListJournals(ctx, domain.ListJournalsFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("ListJournals (default limit) failed: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected all 5 under the default limit, got %d", len(all))
	}
}
