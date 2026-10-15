package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/domain"
	"zoiko.io/payment-proposal-svc/internal/middleware"
)

// TestPgStore_FreezeStoresFingerprint runs against a real, fully migrated
// database: the REVIEW -> FROZEN update stores the fingerprint, the stored
// value equals one recomputed from the frozen rows, and the trigger refuses
// any later change to it.
func TestPgStore_FreezeStoresFingerprint(t *testing.T) {
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
	defer pool.Close()
	s := NewPgStore(pool, zap.NewNop())

	p, err := s.CreateProposal(ctx, tenant, domain.CreateProposalRequest{
		LegalEntityID: uuid.New().String(), PayingBankAccountRef: "acct-1", Currency: "USD",
		PaymentDate: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), PaymentMethod: "ACH",
	}, "maker")
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if p.FrozenFingerprint != "" {
		t.Fatalf("expected no fingerprint before freeze, got %q", p.FrozenFingerprint)
	}

	snap := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.AddItem(ctx, domain.ProposalItem{
		ProposalID: p.ProposalID, PayableSource: domain.SourceAPInvoice, PayableID: "inv-" + uuid.New().String(),
		PayeeRef: "sup-1", GrossAmount: 110, WithholdingAmount: 10, NetAmount: 100, Currency: "USD",
		DueDate: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), PayeeSnapshotAt: &snap, IsActive: true, // no tax id, no exception: the ordinary case
	}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if _, err := s.RecalculateProposal(ctx, p.ProposalID, 110, 10, 100, "maker"); err != nil {
		t.Fatalf("Recalculate: %v", err)
	}
	if _, err := s.SubmitForReview(ctx, p.ProposalID, "maker"); err != nil {
		t.Fatalf("SubmitForReview: %v", err)
	}

	frozen, err := s.FreezeProposal(ctx, p.ProposalID, "checker")
	if err != nil {
		t.Fatalf("FreezeProposal: %v", err)
	}
	if frozen.Status != domain.StatusFrozen || frozen.FrozenFingerprint == "" {
		t.Fatalf("expected FROZEN with a stored fingerprint, got %+v", frozen)
	}

	items, err := s.ListItems(ctx, p.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if live := domain.ComputeFingerprint(frozen, items, frozen.Status); live != frozen.FrozenFingerprint {
		t.Fatalf("stored fingerprint %q != recomputed %q", frozen.FrozenFingerprint, live)
	}

	// The stored fingerprint can never be rewritten once FROZEN.
	if _, err := pool.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenant); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE payment_proposals SET frozen_fingerprint = 'sha256:forged' WHERE proposal_id = $1`, p.ProposalID); err == nil {
		t.Fatal("expected the trigger to refuse rewriting a frozen fingerprint")
	}
	if _, err := conn.Exec(ctx, `UPDATE payment_proposals SET net_amount = 1 WHERE proposal_id = $1`, p.ProposalID); err == nil {
		t.Fatal("expected the trigger to refuse changing a frozen amount")
	}
}
