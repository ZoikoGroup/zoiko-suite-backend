package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

// Domain scenario #19 against real Postgres: a RECEIPT in a UOM other than the
// item's base UOM is refused with ErrUOMMismatch and leaves on-hand untouched.
func TestPgStore_CommitMovement_UOMMismatch_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newActiveTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-UOM-1") // base UOM EACH
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-UOM-1")

	now := time.Now().UTC()
	m := newDraftReceipt(itemID, locID, "idem-uom-db", 5, "")
	m.UOM = "KG"
	m.LegalEntityID = legalEntityID
	if err := s.CreateMovement(ctx, m); err != nil {
		t.Fatalf("CreateMovement failed: %v", err)
	}
	// ValidateMovement may or may not catch it; CommitMovement must.
	if err := s.ValidateMovement(ctx, m.MovementID, now); err == nil {
		if err := s.CommitMovement(ctx, m.MovementID, "preparer-1", now); err != domain.ErrUOMMismatch {
			t.Fatalf("expected ErrUOMMismatch at commit, got %v", err)
		}
	} else if err != domain.ErrUOMMismatch {
		t.Fatalf("expected ErrUOMMismatch at validate, got %v", err)
	}
	if onHand, err := s.GetOnHand(ctx, itemID, locID); err != nil || onHand != 0 {
		t.Fatalf("a refused movement must leave on-hand at 0, got %v (err %v)", onHand, err)
	}
}
