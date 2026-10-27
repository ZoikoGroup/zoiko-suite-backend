package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/middleware"
)

// TestPgStore_PerPayeeInstructionsAndUnknownLifecycle runs against a real,
// fully migrated database: per-payee instructions with payables, write-once
// Banking refs, submit-key binding, PENDING_UNKNOWN round trip, idempotent
// event replay, and AP-08 settlement bookkeeping.
func TestPgStore_PerPayeeInstructionsAndUnknownLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s := NewPgStore(pool, zap.NewNop())

	le := uuid.New().String()
	authID := "auth-" + uuid.New().String()
	run, ins, err := s.CreateRun(ctx, tenant, domain.CreateRunRequest{
		LegalEntityID: le, PayingBankAccountRef: "acct", Currency: "USD", ValueDate: time.Now(), PaymentMethod: "ACH",
	}, []domain.RunInstruction{
		{AuthorizationID: authID, AuthorizationFingerprint: "fp", PayeeRef: "a", NetAmount: 150.25, Currency: "USD",
			Payables: []domain.InstructionPayable{
				{PayableSource: "AP_INVOICE", SourceReference: "inv-1", GrossAmount: 110, WithholdingAmount: 10, NetAmount: 100},
				{PayableSource: "AP_INVOICE", SourceReference: "inv-2", GrossAmount: 50.25, NetAmount: 50.25},
			}},
		{AuthorizationID: authID, AuthorizationFingerprint: "fp", PayeeRef: "b", NetAmount: 200, Currency: "USD",
			Payables: []domain.InstructionPayable{{PayableSource: "EXPENSE_CLAIM", SourceReference: "c-1", GrossAmount: 200, NetAmount: 200}}},
	}, "p1")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(ins) != 2 || len(ins[0].Payables) != 2 {
		t.Fatalf("unexpected create result %+v", ins)
	}

	// same authorization+payee in another run is refused
	if _, _, err := s.CreateRun(ctx, tenant, domain.CreateRunRequest{LegalEntityID: le, PayingBankAccountRef: "acct", Currency: "USD", ValueDate: time.Now(), PaymentMethod: "ACH"},
		[]domain.RunInstruction{{AuthorizationID: authID, PayeeRef: "a", NetAmount: 1, Currency: "USD"}}, "p1"); err != domain.ErrAuthorizationNotEligible {
		t.Fatalf("expected ErrAuthorizationNotEligible, got %v", err)
	}

	list, err := s.ListInstructions(ctx, run.RunID)
	if err != nil || len(list) != 2 || len(list[0].Payables)+len(list[1].Payables) != 3 {
		t.Fatalf("ListInstructions: %v %+v", err, list)
	}

	if _, err := s.ValidateRun(ctx, run.RunID, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LockRun(ctx, run.RunID, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindSubmitKey(ctx, run.RunID, "k1"); err != nil {
		t.Fatalf("BindSubmitKey: %v", err)
	}
	if _, err := s.BindSubmitKey(ctx, run.RunID, "k1"); err != nil {
		t.Fatalf("BindSubmitKey same: %v", err)
	}
	if _, err := s.BindSubmitKey(ctx, run.RunID, "k2"); err != domain.ErrIdempotencyKeyMismatch {
		t.Fatalf("expected mismatch, got %v", err)
	}

	i0 := ins[0].InstructionID
	if err := s.SetInstructionAttemptID(ctx, i0, "att-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstructionAttemptID(ctx, i0, "att-1"); err != nil {
		t.Fatalf("same value should be no-op: %v", err)
	}
	if err := s.SetInstructionAttemptID(ctx, i0, "att-2"); err == nil {
		t.Fatal("expected refusal of a different attempt id")
	}

	u, applied, err := s.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{InstructionID: i0, ExternalStatus: domain.InstructionPendingUnknown, ProviderEventRef: "bnk06:att-1:PENDING_UNKNOWN", Reason: "timeout"}, "p1")
	if err != nil || !applied || u.Status != domain.InstructionPendingUnknown || u.StatusReason != "timeout" {
		t.Fatalf("Reconcile unknown: %v %v %+v", err, applied, u)
	}
	u, applied, err = s.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{InstructionID: i0, ExternalStatus: domain.InstructionPendingUnknown, ProviderEventRef: "bnk06:att-1:PENDING_UNKNOWN", Reason: "timeout"}, "p1")
	if err != nil || applied || u == nil {
		t.Fatalf("replayed event ref must be an idempotent no-op: %v %v", err, applied)
	}
	if _, err := s.SubmitRun(ctx, run.RunID, "k1", "p1"); err != nil {
		t.Fatal(err)
	}
	if r, err := s.UpdateRunAggregateStatus(ctx, run.RunID, domain.StatusPendingUnknown, "p1"); err != nil || r.Status != domain.StatusPendingUnknown {
		t.Fatalf("aggregate unknown: %v", err)
	}
	if _, _, err := s.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{InstructionID: i0, ExternalStatus: domain.InstructionPending, ProviderEventRef: "bnk06:att-1:SUBMITTED"}, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstructionBnk07PaymentID(ctx, i0, "pay-1"); err != nil {
		t.Fatal(err)
	}
	if r, err := s.UpdateRunAggregateStatus(ctx, run.RunID, domain.StatusSubmitted, "p1"); err != nil || r.Status != domain.StatusSubmitted {
		t.Fatalf("aggregate back to submitted: %v", err)
	}

	pend, err := s.ListUnappliedPayables(ctx, i0)
	if err != nil || len(pend) != 2 {
		t.Fatalf("ListUnappliedPayables: %v %d", err, len(pend))
	}
	if err := s.MarkPayableApplied(ctx, i0, "AP_INVOICE", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPayableApplied(ctx, i0, "AP_INVOICE", "inv-1"); err != nil {
		t.Fatalf("second mark should be a no-op: %v", err)
	}
	pend, _ = s.ListUnappliedPayables(ctx, i0)
	if len(pend) != 1 || pend[0].SourceReference != "inv-2" {
		t.Fatalf("expected inv-2 pending, got %+v", pend)
	}
}
