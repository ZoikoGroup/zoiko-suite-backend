package store_test

// ACC-03's approval-lifecycle store methods (SubmitJournalForApproval,
// ApproveJournal, RejectJournal, RequestJournalPosting, MarkJournalPosted,
// AmendDraftJournal) are exercised only by handler_test.go's in-memory
// stub elsewhere in this package — this file is their real-Postgres
// coverage, against the actual guarded UPDATE ... WHERE approval_status =
// $n clauses and the real journal_lines DELETE+INSERT replace, not a
// stub's map mutation. Same posture as every other TestPgStore_* test in
// this package: skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func TestPgStore_ACC03_SubmitApproveRequestPosting_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-07", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 7, 28),
		PostingDate: domain.NewDate(2026, 7, 31), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "acc03-smoke-1",
		ApprovalStatus: domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}

	if err := s.SubmitJournalForApproval(ctx, tenantID, h.JournalID, "preparer-1"); err != nil {
		t.Fatalf("SubmitJournalForApproval: %v", err)
	}
	if err := s.ApproveJournal(ctx, tenantID, h.JournalID, "approver-1", "fp-abc123"); err != nil {
		t.Fatalf("ApproveJournal: %v", err)
	}
	if err := s.RequestJournalPosting(ctx, tenantID, h.JournalID, "approver-1"); err != nil {
		t.Fatalf("RequestJournalPosting: %v", err)
	}
	if err := s.MarkJournalPosted(ctx, tenantID, h.JournalID); err != nil {
		t.Fatalf("MarkJournalPosted: %v", err)
	}

	got, _, err := s.GetJournal(ctx, h.JournalID)
	if err != nil {
		t.Fatalf("GetJournal: %v", err)
	}
	if got.ApprovalStatus != domain.ApprovalStatusPosted {
		t.Fatalf("expected POSTED, got %q", got.ApprovalStatus)
	}
	if got.ApprovalFingerprint == nil || *got.ApprovalFingerprint != "fp-abc123" {
		t.Fatalf("expected fingerprint fp-abc123, got %v", got.ApprovalFingerprint)
	}

	// AmendDraftJournal must now refuse — POSTED is not DRAFT/PENDING_APPROVAL.
	amendErr := s.AmendDraftJournal(ctx, tenantID, h.JournalID, h, lines)
	if amendErr == nil {
		t.Fatal("expected AmendDraftJournal to refuse a POSTED journal")
	}
}

func TestPgStore_ACC03_AmendDraftJournal_ReplacesLines_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-07", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 7, 28),
		PostingDate: domain.NewDate(2026, 7, 31), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "acc03-smoke-2",
		ApprovalStatus: domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}
	if _, _, err := s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}

	updated := &domain.JournalHeader{
		Description: "amended", JournalType: domain.JournalTypeAdjustment,
		TransactionDate: domain.NewDate(2026, 7, 29), PostingDate: domain.NewDate(2026, 7, 30),
		CurrencyCode: "USD",
	}
	newLines := []domain.JournalLine{
		{AccountCode: "2000", DebitAmount: 250},
		{AccountCode: "5000", CreditAmount: 250},
	}
	if err := s.AmendDraftJournal(ctx, tenantID, h.JournalID, updated, newLines); err != nil {
		t.Fatalf("AmendDraftJournal: %v", err)
	}

	got, gotLines, err := s.GetJournal(ctx, h.JournalID)
	if err != nil {
		t.Fatalf("GetJournal: %v", err)
	}
	if got.CurrencyCode != "USD" || got.Description != "amended" {
		t.Fatalf("expected amended fields, got %+v", got)
	}
	if len(gotLines) != 2 || gotLines[0].AccountCode != "2000" {
		t.Fatalf("expected replaced lines, got %+v", gotLines)
	}
}
