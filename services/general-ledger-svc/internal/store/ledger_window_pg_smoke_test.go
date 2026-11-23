package store_test

// QueryLedger's transaction-date window and journal-status narrowing against
// the real database. reconciliation-intelligence-svc reads one cash account's
// FINALIZED entries in a window around a bank statement date to propose the
// book side of an UNMATCHED line, so the window must be inclusive at both
// ends and a REVERSED journal must drop out — bank-reconciliation-svc would
// refuse to match it. Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// cashJournalOn finalizes a journal debiting cash account 1000 by amount on
// the given transaction date.
func cashJournalOn(t *testing.T, ctx context.Context, s *store.PgStore, tenantID, legalEntityID string, txnDate domain.Date, amount float64) *domain.JournalHeader {
	t.Helper()
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-07", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: txnDate,
		PostingDate: txnDate, CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: uuid.New().String(),
		ApprovalStatus: domain.ApprovalStatusPosted,
	}
	lines := []domain.JournalLine{{AccountCode: "1000", DebitAmount: amount}, {AccountCode: "4000", CreditAmount: amount}}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}
	for _, step := range [][2]domain.JournalStatus{
		{domain.JournalStatusPending, domain.JournalStatusValidated},
		{domain.JournalStatusValidated, domain.JournalStatusFinalized},
	} {
		if err := s.TransitionJournal(ctx, tenantID, h.JournalID, step[0], step[1], "preparer-1"); err != nil {
			t.Fatalf("transition to %s: %v", step[1], err)
		}
	}
	return h
}

func journalIDs(entries []domain.LedgerEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[e.JournalID] = true
	}
	return out
}

func TestPgStore_QueryLedger_TransactionDateWindowAndJournalStatus_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entity := uuid.New().String()

	before := cashJournalOn(t, ctx, s, tenantID, entity, domain.NewDate(2026, 7, 9), 100)
	onFrom := cashJournalOn(t, ctx, s, tenantID, entity, domain.NewDate(2026, 7, 10), 100)
	onTo := cashJournalOn(t, ctx, s, tenantID, entity, domain.NewDate(2026, 7, 20), 100)
	after := cashJournalOn(t, ctx, s, tenantID, entity, domain.NewDate(2026, 7, 21), 100)
	reversed := cashJournalOn(t, ctx, s, tenantID, entity, domain.NewDate(2026, 7, 15), 100)

	reversal := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: entity, FiscalPeriod: "2026-07",
		Status: domain.JournalStatusFinalized, JournalType: domain.JournalTypeReversal,
		TransactionDate: domain.NewDate(2026, 7, 16), PostingDate: domain.NewDate(2026, 7, 16), CurrencyCode: "GBP",
		CreatedByPrincipalID: "reverser-1", CorrelationID: uuid.New().String(), ApprovalStatus: domain.ApprovalStatusPosted,
		ReversalOfJournalID: &reversed.JournalID,
	}
	if _, _, err := s.ReverseJournal(ctx, tenantID, reversed.JournalID, reversal,
		[]domain.JournalLine{{AccountCode: "1000", CreditAmount: 100}, {AccountCode: "4000", DebitAmount: 100}}, "reverser-1"); err != nil {
		t.Fatalf("ReverseJournal: %v", err)
	}

	window := domain.QueryLedgerFilter{LegalEntityID: entity, AccountCode: "1000",
		TransactionDateFrom: domain.NewDate(2026, 7, 10), TransactionDateTo: domain.NewDate(2026, 7, 20)}

	all, _, err := s.QueryLedger(ctx, tenantID, window, 100)
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	got := journalIDs(all)
	if !got[onFrom.JournalID] || !got[onTo.JournalID] || !got[reversed.JournalID] || !got[reversal.JournalID] {
		t.Fatalf("window must include both ends, the reversed journal and its reversal; got %v", got)
	}
	if got[before.JournalID] || got[after.JournalID] {
		t.Fatalf("window must exclude dates outside it; got %v", got)
	}
	for _, e := range all {
		if e.AccountCode != "1000" {
			t.Fatalf("account filter leaked %s", e.AccountCode)
		}
	}

	window.JournalStatus = domain.JournalStatusFinalized
	finalized, _, err := s.QueryLedger(ctx, tenantID, window, 100)
	if err != nil {
		t.Fatalf("QueryLedger FINALIZED: %v", err)
	}
	got = journalIDs(finalized)
	if got[reversed.JournalID] {
		t.Fatalf("a REVERSED journal must drop out under journal_status=FINALIZED")
	}
	if !got[onFrom.JournalID] || !got[onTo.JournalID] || !got[reversal.JournalID] {
		t.Fatalf("FINALIZED journals in the window must remain; got %v", got)
	}

	window.JournalStatus = domain.JournalStatusReversed
	rev, _, err := s.QueryLedger(ctx, tenantID, window, 100)
	if err != nil {
		t.Fatalf("QueryLedger REVERSED: %v", err)
	}
	if got := journalIDs(rev); len(got) != 1 || !got[reversed.JournalID] {
		t.Fatalf("journal_status=REVERSED must return only the reversed journal; got %v", got)
	}
}

// TestPgStore_QueryLedger_CursorPagesThroughEveryEntryExactlyOnce proves A15's
// keyset cursor against a real database: hasMore signals truncation, and
// following AfterEntrySeq across pages returns every entry exactly once, in
// entry_seq order, regardless of how the page boundary falls relative to one
// journal's own two lines.
func TestPgStore_QueryLedger_CursorPagesThroughEveryEntryExactlyOnce(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entity := uuid.New().String()

	const journalCount = 5 // 10 ledger entries (2 lines each)
	for i := 0; i < journalCount; i++ {
		cashJournalOn(t, ctx, s, tenantID, entity, domain.NewDate(2026, 7, 10), float64(100+i))
	}
	filter := domain.QueryLedgerFilter{LegalEntityID: entity}

	first, hasMore, err := s.QueryLedger(ctx, tenantID, filter, 3)
	if err != nil {
		t.Fatalf("QueryLedger page 1: %v", err)
	}
	if len(first) != 3 || !hasMore {
		t.Fatalf("expected a 3-row page with hasMore, got %d rows, hasMore=%v", len(first), hasMore)
	}

	seen := map[string]bool{}
	for _, e := range first {
		seen[e.LedgerEntryID] = true
	}
	after := first[len(first)-1].EntrySeq
	lastSeq := after
	for {
		filter.AfterEntrySeq = &after
		page, more, err := s.QueryLedger(ctx, tenantID, filter, 3)
		if err != nil {
			t.Fatalf("QueryLedger page after %d: %v", after, err)
		}
		for _, e := range page {
			if seen[e.LedgerEntryID] {
				t.Fatalf("entry %s returned on two pages", e.LedgerEntryID)
			}
			seen[e.LedgerEntryID] = true
			if e.EntrySeq <= lastSeq {
				t.Fatalf("entry_seq must strictly increase across pages: %d after %d", e.EntrySeq, lastSeq)
			}
			lastSeq = e.EntrySeq
		}
		if !more {
			if len(page) == 0 {
				t.Fatalf("the final page must still carry its own rows, not be empty")
			}
			break
		}
		after = page[len(page)-1].EntrySeq
	}
	if len(seen) != journalCount*2 {
		t.Fatalf("expected every one of %d entries exactly once, got %d", journalCount*2, len(seen))
	}
}
