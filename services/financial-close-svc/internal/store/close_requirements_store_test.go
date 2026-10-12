package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func newRequirement(tenantID, entity, kind, subledger, book, bank, reason string) *domain.CloseRequirement {
	return &domain.CloseRequirement{
		RequirementID: uuid.NewString(), TenantID: tenantID, LegalEntityID: entity, Kind: kind,
		Subledger: subledger, BookID: book, BankAccountID: bank, Reason: reason,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "controller-1",
	}
}

func TestCloseRequirements_AddListReplayRemove(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	assets := newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "ASSETS", "STATUTORY", "", "")
	created, err := s.CreateCloseRequirement(ctx, assets)
	if err != nil || !created {
		t.Fatalf("add ASSETS: created=%v err=%v", created, err)
	}
	excl := newRequirement(tenantID, "le-1", domain.CloseRequirementBankAccountExclusion, "", "", "acct-petty-cash", "petty cash float under £200, counted monthly")
	if _, err := s.CreateCloseRequirement(ctx, excl); err != nil {
		t.Fatalf("add exclusion: %v", err)
	}

	// Adding the same requirement again is a replay, not a second row.
	again := newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "ASSETS", "STATUTORY", "", "")
	created, err = s.CreateCloseRequirement(ctx, again)
	if err != nil || created || again.RequirementID != assets.RequirementID {
		t.Fatalf("replay: created=%v err=%v id=%s want %s", created, err, again.RequirementID, assets.RequirementID)
	}
	// A different book is a different requirement.
	tax := newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "ASSETS", "TAX", "", "")
	if created, err := s.CreateCloseRequirement(ctx, tax); err != nil || !created {
		t.Fatalf("ASSETS@TAX should be its own requirement: created=%v err=%v", created, err)
	}

	list, err := s.ListCloseRequirements(ctx, "le-1")
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %d items, err %v; want 3", len(list), err)
	}

	removed, err := s.RemoveCloseRequirement(ctx, tax.RequirementID, "controller-2", "tax book reconciled by the tax team instead", time.Now().UTC())
	if err != nil || removed.RemovedAt == nil || removed.RemovedByPrincipalID != "controller-2" {
		t.Fatalf("remove: %+v err %v", removed, err)
	}
	if list, _ := s.ListCloseRequirements(ctx, "le-1"); len(list) != 2 {
		t.Fatalf("a removed requirement must leave the active checklist, got %d", len(list))
	}
	// Still retrievable as evidence.
	got, err := s.GetCloseRequirement(ctx, tax.RequirementID)
	if err != nil || got.RemovalReason != "tax book reconciled by the tax team instead" {
		t.Fatalf("removed requirement not kept as evidence: %+v %v", got, err)
	}
	if _, err := s.RemoveCloseRequirement(ctx, tax.RequirementID, "controller-2", "again", time.Now().UTC()); !errors.Is(err, domain.ErrCloseRequirementNotFound) {
		t.Fatalf("second removal: err %v, want ErrCloseRequirementNotFound", err)
	}
	// After removal the same requirement can be added again.
	readd := newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "ASSETS", "TAX", "", "")
	if created, err := s.CreateCloseRequirement(ctx, readd); err != nil || !created {
		t.Fatalf("re-add after removal: created=%v err=%v", created, err)
	}
}

// The database itself refuses what the checklist must never hold, whatever
// the handler does.
func TestCloseRequirements_DatabaseRefusesInvalidShapes(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	cases := map[string]*domain.CloseRequirement{
		"AR is baseline, not a checklist item":   newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "AR", "", "", ""),
		"ASSETS without its book":                newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "ASSETS", "", "", ""),
		"book on a non-ASSETS control":           newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "INVENTORY_VALUE", "STATUTORY", "", ""),
		"exclusion without a reason":             newRequirement(tenantID, "le-1", domain.CloseRequirementBankAccountExclusion, "", "", "acct-1", "  "),
		"exclusion without an account":           newRequirement(tenantID, "le-1", domain.CloseRequirementBankAccountExclusion, "", "", "", "immaterial"),
		"unknown kind":                           newRequirement(tenantID, "le-1", "WAIVE_EVERYTHING", "", "", "", "x"),
		"control that also names a bank account": newRequirement(tenantID, "le-1", domain.CloseRequirementSubledgerControl, "STOCK_COUNT", "", "acct-1", ""),
	}
	for name, cr := range cases {
		if _, err := s.CreateCloseRequirement(ctx, cr); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
}

// A requirement is evidence: only its removal stamp may ever change, once.
func TestCloseRequirements_RowsAreImmutableAndNeverDeleted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	cr := newRequirement(tenantID, "le-1", domain.CloseRequirementBankAccountExclusion, "", "", "acct-1", "dormant, zero balance")
	if _, err := s.CreateCloseRequirement(ctx, cr); err != nil {
		t.Fatal(err)
	}

	exec := func(sql string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql, cr.RequirementID)
		return err
	}
	if err := exec(`UPDATE close_requirements SET reason = 'rewritten later' WHERE requirement_id = $1`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("rewriting the reason: err %v, want the immutability guard", err)
	}
	if err := exec(`DELETE FROM close_requirements WHERE requirement_id = $1`); err == nil ||
		!strings.Contains(err.Error(), "never deleted") {
		t.Fatalf("delete: err %v, want the no-delete guard", err)
	}
}

func TestCloseRequirements_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	a, b := uuid.NewString(), uuid.NewString()
	ctxA := svcmiddleware.WithTenant(context.Background(), a)
	ctxB := svcmiddleware.WithTenant(context.Background(), b)
	cr := newRequirement(a, "le-shared-name", domain.CloseRequirementSubledgerControl, "INVENTORY_VALUE", "", "", "")
	if _, err := s.CreateCloseRequirement(ctxA, cr); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListCloseRequirements(ctxB, "le-shared-name"); err != nil || len(list) != 0 {
		t.Fatalf("tenant B sees tenant A's checklist: %v %v", list, err)
	}
	if _, err := s.GetCloseRequirement(ctxB, cr.RequirementID); !errors.Is(err, domain.ErrCloseRequirementNotFound) {
		t.Fatalf("tenant B fetched tenant A's requirement: %v", err)
	}
	if _, err := s.RemoveCloseRequirement(ctxB, cr.RequirementID, "intruder", "x", time.Now().UTC()); !errors.Is(err, domain.ErrCloseRequirementNotFound) {
		t.Fatalf("tenant B removed tenant A's requirement: %v", err)
	}
}
