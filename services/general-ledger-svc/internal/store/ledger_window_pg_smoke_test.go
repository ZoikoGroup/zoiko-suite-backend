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

	all, err := s.QueryLedger(ctx, tenantID, window, 100)
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
	finalized, err := s.QueryLedger(ctx, tenantID, window, 100)
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
	rev, err := s.QueryLedger(ctx, tenantID, window, 100)
	if err != nil {
		t.Fatalf("QueryLedger REVERSED: %v", err)
	}
	if got := journalIDs(rev); len(got) != 1 || !got[reversed.JournalID] {
		t.Fatalf("journal_status=REVERSED must return only the reversed journal; got %v", got)
	}
}
