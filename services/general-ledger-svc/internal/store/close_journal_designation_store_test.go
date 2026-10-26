package store_test

// Migration 000027 adds the journal-level close-journal designation that
// ACC-14's CLOSE_REVIEW policy consults. These tests prove the column survives
// a real Postgres round trip, that the database itself refuses an unrecognised
// designation or one that disagrees with journal_type, and that the down
// migration refuses to erase a designation that a journal still depends on.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func newDesignatedHeader(tenantID, legalEntityID, correlationID, designation string, journalType domain.JournalType) *domain.JournalHeader {
	return &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-10", Status: domain.JournalStatusPending,
		JournalType: journalType, TransactionDate: domain.NewDate(2026, 10, 30),
		PostingDate: domain.NewDate(2026, 10, 30), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: correlationID,
		ApprovalStatus:          domain.ApprovalStatusDraft,
		CloseJournalDesignation: &designation,
	}
}

func TestPgStore_CloseJournalDesignation_RoundTripsAndEmitsEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	h := newDesignatedHeader(tenantID, legalEntityID, "close-journal-designation-smoke-1",
		domain.CloseJournalDesignationAccrualRecognition, domain.JournalTypeAccrual)
	lines := []domain.JournalLine{
		{AccountCode: "6100", DebitAmount: 100},
		{AccountCode: "2100", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}

	// A later request reloads the header from Postgres. If the column is
	// missing from journalHeaderColumns, scanHeaderTargets or insertJournal's
	// INSERT list, CLOSE_REVIEW's designation is silently lost and a
	// legitimately designated close journal can no longer post.
	got, _, err := s.GetJournal(ctx, h.JournalID)
	if err != nil {
		t.Fatalf("GetJournal: %v", err)
	}
	if got == nil || got.CloseJournalDesignation == nil {
		t.Fatal("CloseJournalDesignation did not survive a reload from Postgres")
	}
	if *got.CloseJournalDesignation != domain.CloseJournalDesignationAccrualRecognition {
		t.Fatalf("CloseJournalDesignation = %q, want %q", *got.CloseJournalDesignation,
			domain.CloseJournalDesignationAccrualRecognition)
	}

	// The designation is audit evidence that the posting was admitted as a
	// named close workflow, not merely by a permission check. It must appear
	// in the created event alongside the journal.
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE event_type LIKE '%journal.created%' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&payload); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	if !strings.Contains(payload, domain.CloseJournalDesignationAccrualRecognition) {
		t.Fatalf("journal.created outbox payload does not contain the close-journal designation: %s", payload)
	}
}

func TestPgStore_CloseJournalDesignation_AbsentReadsBackAsNil(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-10", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 10, 15),
		PostingDate: domain.NewDate(2026, 10, 15), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "close-journal-designation-smoke-2",
		ApprovalStatus: domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 50},
		{AccountCode: "4000", CreditAmount: 50},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}

	got, _, err := s.GetJournal(ctx, h.JournalID)
	if err != nil {
		t.Fatalf("GetJournal: %v", err)
	}
	if got.CloseJournalDesignation != nil {
		t.Fatalf("CloseJournalDesignation = %q, want nil for an ordinary journal", *got.CloseJournalDesignation)
	}
}

func TestPgStore_CloseJournalDesignation_DatabaseRejectsInvalidValues(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()
	lines := []domain.JournalLine{
		{AccountCode: "6100", DebitAmount: 100},
		{AccountCode: "2100", CreditAmount: 100},
	}

	cases := []struct {
		name        string
		designation string
		journalType domain.JournalType
	}{
		{
			name:        "unrecognised designation",
			designation: "NOT_A_CLOSE_WORKFLOW",
			journalType: domain.JournalTypeAccrual,
		},
		{
			name:        "designation disagrees with journal type",
			designation: domain.CloseJournalDesignationAccrualRecognition,
			journalType: domain.JournalTypeStandard,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDesignatedHeader(tenantID, legalEntityID, "close-journal-designation-invalid-"+tc.name,
				tc.designation, tc.journalType)
			if _, _, err := s.CreateJournal(ctx, h, lines); err == nil {
				t.Fatal("database accepted an invalid close-journal designation; the CHECK constraint is missing or wrong")
			}
		})
	}
}

func TestMigration000027_DownRefusesWhileDesignationExists(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	h := newDesignatedHeader(tenantID, legalEntityID, "close-journal-designation-down-1",
		domain.CloseJournalDesignationPrepaymentRecognition, domain.JournalTypeAccrual)
	lines := []domain.JournalLine{
		{AccountCode: "6200", DebitAmount: 100},
		{AccountCode: "1400", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}

	_, filename, _, _ := runtime.Caller(0)
	migrationDir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	downSQL, err := os.ReadFile(filepath.Join(migrationDir, "000027_add_close_journal_designation.down.sql"))
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	upSQL, err := os.ReadFile(filepath.Join(migrationDir, "000027_add_close_journal_designation.up.sql"))
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}

	// The guard must see the row and refuse. Wrap the migration in a
	// transaction because its SET LOCAL row_security = off is transaction-
	// scoped.
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = tx.Exec(context.Background(), string(downSQL))
	_ = tx.Rollback(context.Background())
	if err == nil {
		t.Fatal("down migration succeeded while a journal still carried a close-journal designation")
	}
	if !strings.Contains(err.Error(), "cannot roll back 000027") {
		t.Fatalf("down migration refused for the wrong reason: %v", err)
	}

	// Clear the dependency, then the same down migration must succeed. This
	// must COMMIT (scoped() deliberately rolls back, which is right for a
	// read-only assertion but would leave the row untouched here).
	tx, err = pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin clear: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		`SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("scope clear: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		`UPDATE journal_headers SET close_journal_designation = NULL WHERE journal_id = $1`, h.JournalID); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("clear designation: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit clear: %v", err)
	}
	tx, err = pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(), string(downSQL)); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("down migration after clearing the designation: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit down migration: %v", err)
	}

	// Leave the schema as the rest of the suite expects it.
	if _, err := pool.Exec(context.Background(), string(upSQL)); err != nil {
		t.Fatalf("reapply up migration: %v", err)
	}
}
