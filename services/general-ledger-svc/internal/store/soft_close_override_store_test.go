package store_test

// SoftCloseOverrideReason (migration 000015) is a nullable *string on
// JournalHeader that several handler.go call sites re-check after a fresh
// store.GetJournal — PostJournal, PostApprovedJournal, and the reprocess
// flow's adoptExistingJournal all reload the header from Postgres and then
// branch on header.SoftCloseOverrideReason being set, to decide whether a
// soft-close exception that was authorized at journal creation still
// applies at post time. If the column is missing from journalHeaderColumns,
// scanHeaderTargets, or insertJournal's own INSERT list, the reason is
// silently dropped on write (or on read) and every one of those later
// re-checks sees nil — meaning a legitimately authorized soft-close
// exception cannot survive past the single request that created it, and
// PostJournal (which accepts no request body of its own to supply a fresh
// override) would then refuse to ever post that journal while the period
// stays in SOFT_CLOSE. This is exactly that round trip, proven against a
// real column, not a stub map.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func TestPgStore_SoftCloseOverrideReason_RoundTripsThroughGetJournal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	reason := "late supplier credit note required before month-end sign-off"
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-10", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 10, 30),
		PostingDate: domain.NewDate(2026, 10, 30), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "soft-close-override-smoke-1",
		ApprovalStatus:          domain.ApprovalStatusDraft,
		SoftCloseOverrideReason: &reason,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}

	// A fresh load — exactly what PostJournal, PostApprovedJournal and
	// adoptExistingJournal each do in a later, separate request — must still
	// see the reason that was declared at creation.
	got, _, err := s.GetJournal(ctx, h.JournalID)
	if err != nil {
		t.Fatalf("GetJournal: %v", err)
	}
	if got == nil {
		t.Fatal("GetJournal returned nil for a journal that was just created")
	}
	if got.SoftCloseOverrideReason == nil {
		t.Fatal("SoftCloseOverrideReason did not survive a reload from Postgres — it is nil; " +
			"every handler path that re-checks it after a fresh GetJournal will wrongly conclude " +
			"no override was ever authorized")
	}
	if *got.SoftCloseOverrideReason != reason {
		t.Fatalf("SoftCloseOverrideReason = %q, want %q", *got.SoftCloseOverrideReason, reason)
	}

	// migration 000015's own doc comment promises the reason is "emitted in
	// the JournalCreated outbox event for audit traceability" — prove it
	// actually is, not just that the comment says so.
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE event_type LIKE '%journal.created%' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&payload); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	if !strings.Contains(payload, reason) {
		t.Fatalf("journal.created outbox payload does not contain the soft-close override reason: %s", payload)
	}
}

// A journal created with NO override must read back as nil, not as an
// empty-but-non-nil string — handler.go's own checks are
// `header.SoftCloseOverrideReason != nil`, so a false "not nil" here would
// make the override look declared when it never was.
func TestPgStore_SoftCloseOverrideReason_AbsentReadsBackAsNil(t *testing.T) {
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
		CreatedByPrincipalID: "preparer-1", CorrelationID: "soft-close-override-smoke-2",
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
	if got.SoftCloseOverrideReason != nil {
		t.Fatalf("SoftCloseOverrideReason = %q, want nil for a journal created with no override",
			*got.SoftCloseOverrideReason)
	}
}

