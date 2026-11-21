//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// intercompany-accounting-svc SPEC_DEVIATIONS A5: "There is no
// intercompany-account resolution ('Server-resolved: intercompany
// accounts')... Needs an intercompany account designation from the chart of
// accounts." These tests prove IsIntercompanyAccount follows the exact same
// maker-checker shape as IsControlAccount (A4/B16, exercised in
// acc01_maker_checker_integration_test.go): a change touching it is not
// applied in one call — it is a pending AccountChangeApproval a different
// principal must approve, and the same principal cannot propose and approve
// it.

func newIntercompanyAccount(tenantID, code, name string) *domain.Account {
	a := newTestAccount(tenantID, code, name)
	a.IsIntercompanyAccount = true
	return a
}

// Flagging a previously-ordinary account as intercompany is itself the
// material change (B16's "either side of the change"): cur.IsIntercompanyAccount
// is false but updated.IsIntercompanyAccount is true.
func TestPgStore_AmendMarksIntercompanyAccount_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	clearing := newTestAccount(tenantID, "1890", "Intercompany Clearing")
	if err := s.CreateAccount(ctx, clearing); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if clearing.IsIntercompanyAccount {
		t.Fatal("precondition: account must not start out classified intercompany")
	}

	updates := *clearing
	updates.IsIntercompanyAccount = true
	unchanged, pending, _, err := s.AmendAccountVersion(ctx, tenantID, "1890", uuid.NewString(),
		1, time.Now().UTC(), &updates, "classifying as intercompany clearing", nil, "", "preparer-1")
	if err != nil {
		t.Fatalf("AmendAccountVersion: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending AccountChangeApproval when is_intercompany_account is changing")
	}
	if unchanged.IsIntercompanyAccount {
		t.Fatal("a pending amendment must not apply the reclassification yet")
	}
	if unchanged.Version != 1 {
		t.Fatalf("a pending amendment must not advance the version, got %d", unchanged.Version)
	}

	// Same principal proposed and may not approve — exactly A4/B16's gate.
	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "preparer-1", uuid.NewString()); !errors.Is(err, domain.ErrAccountChangeSelfApproval) {
		t.Fatalf("expected ErrAccountChangeSelfApproval for a same-principal approval, got %v", err)
	}

	// A different principal succeeds, and the flag is now applied for real.
	approved, replay, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString())
	if err != nil {
		t.Fatalf("ApproveAccountChange: %v", err)
	}
	if replay {
		t.Fatal("the first approval must not report a replay")
	}
	if !approved.IsIntercompanyAccount {
		t.Fatal("expected is_intercompany_account=true after approval")
	}
	if approved.Version != 2 {
		t.Fatalf("expected version 2 after approval, got %d", approved.Version)
	}

	after, err := s.GetAccountByCode(ctx, tenantID, "1890")
	if err != nil {
		t.Fatalf("GetAccountByCode: %v", err)
	}
	if !after.IsIntercompanyAccount {
		t.Fatal("GetAccountByCode must reflect the approved reclassification")
	}
}

// Un-flagging an already-intercompany account is the same material change in
// the other direction, and is gated the same way.
func TestPgStore_UnmarksIntercompanyAccount_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	clearing := newIntercompanyAccount(tenantID, "1890", "Intercompany Clearing")
	if err := s.CreateAccount(ctx, clearing); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	updates := *clearing
	updates.IsIntercompanyAccount = false
	_, pending, _, err := s.AmendAccountVersion(ctx, tenantID, "1890", uuid.NewString(),
		1, time.Now().UTC(), &updates, "no longer an intercompany clearing account", nil, "", "preparer-1")
	if err != nil {
		t.Fatalf("AmendAccountVersion: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending AccountChangeApproval when un-marking is_intercompany_account")
	}

	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString()); err != nil {
		t.Fatalf("ApproveAccountChange: %v", err)
	}
	after, err := s.GetAccountByCode(ctx, tenantID, "1890")
	if err != nil {
		t.Fatalf("GetAccountByCode: %v", err)
	}
	if after.IsIntercompanyAccount {
		t.Fatal("expected is_intercompany_account=false after approval")
	}
}

// Reparenting an account already classified intercompany is material even
// though the reparent itself never touches the flag — the same reasoning
// A4/B16 already applies to a control account's reparent.
func TestPgStore_ReparentIntercompanyAccount_RequiresApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	clearing := newIntercompanyAccount(tenantID, "1890", "Intercompany Clearing")
	if err := s.CreateAccount(ctx, clearing); err != nil {
		t.Fatalf("CreateAccount(clearing): %v", err)
	}
	newParent := newTestAccount(tenantID, "1900", "Other Assets")
	if err := s.CreateAccount(ctx, newParent); err != nil {
		t.Fatalf("CreateAccount(newParent): %v", err)
	}

	_, pending, _, err := s.ReparentAccount(ctx, tenantID, "1890", uuid.NewString(),
		&newParent.AccountID, 1, time.Time{}, "reorganising the chart", nil, "", "preparer-1")
	if err != nil {
		t.Fatalf("ReparentAccount: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending AccountChangeApproval when reparenting an intercompany account")
	}

	if _, _, err := s.ApproveAccountChange(ctx, tenantID, pending.ChangeID, "approver-1", uuid.NewString()); err != nil {
		t.Fatalf("ApproveAccountChange: %v", err)
	}
	after, err := s.GetAccountByCode(ctx, tenantID, "1890")
	if err != nil {
		t.Fatalf("GetAccountByCode: %v", err)
	}
	if after.ParentAccountID == nil || *after.ParentAccountID != newParent.AccountID {
		t.Fatalf("expected parent %s after approval, got %v", newParent.AccountID, after.ParentAccountID)
	}
	if !after.IsIntercompanyAccount {
		t.Fatal("the reparent must not have cleared the classification")
	}
}

// An ordinary amendment that never touches is_intercompany_account on a
// non-control, non-intercompany account is completely unaffected: no
// approval required, exactly as before this change.
func TestPgStore_AmendOrdinaryAccount_IntercompanyFlagUnchanged_NoApprovalRequired(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	ordinary := newTestAccount(tenantID, "7000", "Office Supplies")
	if err := s.CreateAccount(ctx, ordinary); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	updates := *ordinary
	updates.AccountName = "Office Supplies (renamed)"
	result, pending, _, err := s.AmendAccountVersion(ctx, tenantID, "7000", uuid.NewString(),
		1, time.Now().UTC(), &updates, "", nil, "", "preparer-1")
	if err != nil {
		t.Fatalf("AmendAccountVersion: %v", err)
	}
	if pending != nil {
		t.Fatal("an ordinary rename must not require approval")
	}
	if result.AccountName != "Office Supplies (renamed)" {
		t.Fatalf("expected the rename applied immediately, got %q", result.AccountName)
	}
}
