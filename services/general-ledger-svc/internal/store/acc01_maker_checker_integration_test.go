//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// SPEC_DEVIATIONS A4/B16: "Creator cannot approve material control-account
// or hierarchy change where maker-checker is configured." These tests prove
// the gate ReparentAccount/AmendAccountVersion/ApproveAccountChange add on
// top of ACC-01's ordinary, ungated path — exercised in
// acc01_account_versioning_integration_test.go — fires only when a control
// account is on either side of the change, and that it is a real two-
// principal control, not just a status label.

func newControlAccount(tenantID, code, name string) *domain.Account {
	a := newTestAccount(tenantID, code, name)
	a.IsControlAccount = true
	return a
}

func TestPgStore_ReparentControlAccount_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	bank := newControlAccount(tenantID, "1000", "Bank Suspense")
	if err := s.CreateAccount(ctx, bank); err != nil {
		t.Fatalf("CreateAccount(bank): %v", err)
	}
	newParent := newTestAccount(tenantID, "1900", "Other Assets")
	if err := s.CreateAccount(ctx, newParent); err != nil {
		t.Fatalf("CreateAccount(newParent): %v", err)
	}

	moved, pending, _, err := s.ReparentAccount(ctx, tenantID, "1000", uuid.NewString(),
		&newParent.AccountID, 1, time.Time{}, "moving bank suspense", nil, "preparer-1")
	if err != nil {
		t.Fatalf("ReparentAccount: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending AccountChangeApproval for a control-account reparent")
	}
	if pending.Status != domain.AccountChangeStatusPending {
		t.Fatalf("expected status PENDING, got %s", pending.Status)
	}
	if moved.ParentAccountID != nil {
		t.Fatal("a pending reparent must not move the account yet")
	}
	if moved.Version != 1 {
		t.Fatalf("a pending reparent must not advance the version, got %d", moved.Version)
	}

	// The preparer cannot also be the approver.
	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "preparer-1", uuid.NewString()); !errors.Is(err, domain.ErrAccountChangeSelfApproval) {
		t.Fatalf("expected ErrAccountChangeSelfApproval for a self-approval, got %v", err)
	}

	approved, replay, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString())
	if err != nil {
		t.Fatalf("ApproveAccountChange: %v", err)
	}
	if replay {
		t.Fatal("the first approval must not report a replay")
	}
	if approved.ParentAccountID == nil || *approved.ParentAccountID != newParent.AccountID {
		t.Fatalf("expected parent %s after approval, got %v", newParent.AccountID, approved.ParentAccountID)
	}
	if approved.Version != 2 {
		t.Fatalf("expected version 2 after approval, got %d", approved.Version)
	}

	// A second approval of the same (now APPROVED) change is refused, not
	// silently re-applied.
	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-2", uuid.NewString()); !errors.Is(err, domain.ErrAccountChangeNotPending) {
		t.Fatalf("expected ErrAccountChangeNotPending on a second approval, got %v", err)
	}

	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		var count int
		if err := tx.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM eventing_outbox
			 WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3`,
			tenantID, "com.zoikosuite.accounting.account.hierarchy-changed", bank.AccountID).Scan(&count); err != nil {
			t.Fatalf("count eventing_outbox: %v", err)
		}
		// One pending proposal that was refused (self-approval) must not have
		// emitted anything; exactly the one real approval should have.
		if count != 1 {
			t.Fatalf("hierarchy-changed events = %d, want 1 (only the real approval, not the refused self-approval attempt)", count)
		}
	})
}

func TestPgStore_ReparentOntoControlAccountParent_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	controlParent := newControlAccount(tenantID, "1000", "Intercompany Clearing")
	if err := s.CreateAccount(ctx, controlParent); err != nil {
		t.Fatalf("CreateAccount(controlParent): %v", err)
	}
	ordinary := newTestAccount(tenantID, "7000", "Office Supplies")
	if err := s.CreateAccount(ctx, ordinary); err != nil {
		t.Fatalf("CreateAccount(ordinary): %v", err)
	}

	// The account being moved is not a control account, but its destination
	// is — still material under the spec's own "control-account ... change"
	// wording, since it changes what rolls up under the control account.
	_, pending, _, err := s.ReparentAccount(ctx, tenantID, "7000", uuid.NewString(),
		&controlParent.AccountID, 1, time.Time{}, "folding into clearing", nil, "preparer-1")
	if err != nil {
		t.Fatalf("ReparentAccount: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending AccountChangeApproval when the new parent is a control account")
	}

	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString()); err != nil {
		t.Fatalf("ApproveAccountChange: %v", err)
	}
	after, err := s.GetAccountByCode(ctx, tenantID, "7000")
	if err != nil {
		t.Fatalf("GetAccountByCode: %v", err)
	}
	if after.ParentAccountID == nil || *after.ParentAccountID != controlParent.AccountID {
		t.Fatalf("expected parent %s after approval, got %v", controlParent.AccountID, after.ParentAccountID)
	}
}

func TestPgStore_AmendControlAccount_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	receivable := newControlAccount(tenantID, "1200", "Accounts Receivable Control")
	if err := s.CreateAccount(ctx, receivable); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	updates := *receivable
	updates.AccountName = "Trade Receivables Control"
	unchanged, pending, _, err := s.AmendAccountVersion(ctx, tenantID, "1200", uuid.NewString(),
		1, time.Now().UTC(), &updates, "renaming for clarity", nil, "preparer-1")
	if err != nil {
		t.Fatalf("AmendAccountVersion: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending AccountChangeApproval for a control-account amendment")
	}
	if unchanged.AccountName != "Accounts Receivable Control" {
		t.Fatalf("a pending amendment must not rename the account yet, got %q", unchanged.AccountName)
	}

	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "preparer-1", uuid.NewString()); !errors.Is(err, domain.ErrAccountChangeSelfApproval) {
		t.Fatalf("expected ErrAccountChangeSelfApproval, got %v", err)
	}

	approved, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString())
	if err != nil {
		t.Fatalf("ApproveAccountChange: %v", err)
	}
	if approved.AccountName != "Trade Receivables Control" {
		t.Fatalf("expected the amendment applied after approval, got %q", approved.AccountName)
	}
}

// A stale expected_version by the time of approval (the account moved on
// in between) is refused exactly as a direct call would refuse it — an
// approval cannot silently apply a decision made against out-of-date facts.
func TestPgStore_ApproveAccountChange_StaleVersionConflict(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	cash := newControlAccount(tenantID, "1000", "Cash")
	if err := s.CreateAccount(ctx, cash); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	parent := newTestAccount(tenantID, "1900", "Other Assets")
	if err := s.CreateAccount(ctx, parent); err != nil {
		t.Fatalf("CreateAccount(parent): %v", err)
	}

	_, pending, _, err := s.ReparentAccount(ctx, tenantID, "1000", uuid.NewString(),
		&parent.AccountID, 1, time.Time{}, "moving cash", nil, "preparer-1")
	if err != nil {
		t.Fatalf("ReparentAccount: %v", err)
	}

	// The account moves on (an unrelated, ungated status change — not another
	// material change, which would instead collide with the one already
	// pending) before the reparent is approved.
	if _, err := s.DeactivateAccount(ctx, tenantID, "1000", 1, time.Time{}, "temporarily suspended", nil, "preparer-2"); err != nil {
		t.Fatalf("DeactivateAccount: %v", err)
	}

	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString()); !errors.Is(err, domain.ErrAccountVersionConflict) {
		t.Fatalf("expected ErrAccountVersionConflict for a stale pending change, got %v", err)
	}
}

// SPEC_DEVIATIONS A8: "reason ... retained with every material change."
// reason stays optional everywhere else (A8 is otherwise still open); this
// is the one place this codebase has a formal definition of "material".
func TestPgStore_MaterialChange_RequiresReason(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	cash := newControlAccount(tenantID, "1000", "Cash")
	if err := s.CreateAccount(ctx, cash); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	parent := newTestAccount(tenantID, "1900", "Other Assets")
	if err := s.CreateAccount(ctx, parent); err != nil {
		t.Fatalf("CreateAccount(parent): %v", err)
	}

	if _, _, _, err := s.ReparentAccount(ctx, tenantID, "1000", uuid.NewString(),
		&parent.AccountID, 1, time.Time{}, "", nil, "preparer-1"); !errors.Is(err, domain.ErrAccountChangeReasonRequired) {
		t.Fatalf("expected ErrAccountChangeReasonRequired for a blank reason, got %v", err)
	}
	if _, _, _, err := s.ReparentAccount(ctx, tenantID, "1000", uuid.NewString(),
		&parent.AccountID, 1, time.Time{}, "   ", nil, "preparer-1"); !errors.Is(err, domain.ErrAccountChangeReasonRequired) {
		t.Fatalf("expected ErrAccountChangeReasonRequired for a whitespace-only reason, got %v", err)
	}

	updates := *cash
	updates.AccountName = "Petty Cash"
	if _, _, _, err := s.AmendAccountVersion(ctx, tenantID, "1000", uuid.NewString(),
		1, time.Now().UTC(), &updates, "", nil, "preparer-1"); !errors.Is(err, domain.ErrAccountChangeReasonRequired) {
		t.Fatalf("expected ErrAccountChangeReasonRequired for a blank amendment reason, got %v", err)
	}

	// An ordinary, non-control account is completely unaffected: no reason
	// required, applied immediately, exactly as before this change.
	ordinary := newTestAccount(tenantID, "7000", "Office Supplies")
	if err := s.CreateAccount(ctx, ordinary); err != nil {
		t.Fatalf("CreateAccount(ordinary): %v", err)
	}
	ordinaryUpdates := *ordinary
	ordinaryUpdates.AccountName = "Office Supplies (renamed)"
	if _, pending, _, err := s.AmendAccountVersion(ctx, tenantID, "7000", uuid.NewString(),
		1, time.Now().UTC(), &ordinaryUpdates, "", nil, "preparer-1"); err != nil || pending != nil {
		t.Fatalf("an ordinary account's amendment must not require a reason, got pending=%v err=%v", pending, err)
	}
}

func TestPgStore_GetAccountChangeApproval_NotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	if _, err := s.GetAccountChangeApproval(ctx, tenantID, uuid.NewString()); !errors.Is(err, domain.ErrAccountChangeNotFound) {
		t.Fatalf("expected ErrAccountChangeNotFound, got %v", err)
	}
}
