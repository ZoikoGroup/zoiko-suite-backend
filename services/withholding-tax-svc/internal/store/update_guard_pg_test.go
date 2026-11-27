package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/withholding-tax-svc/internal/domain"
	"zoiko.io/withholding-tax-svc/internal/middleware"
	"zoiko.io/withholding-tax-svc/internal/store"
)

// Update used to recompute and overwrite the amounts of a REMITTED (or
// CANCELLED) obligation.
func TestPgStore_Update_RefusedOnceRemittedOrCancelled(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-wht")
	s := store.NewPgStore(pool)
	mk := func(ref string) *domain.WithholdingTaxObligation {
		return &domain.WithholdingTaxObligation{LegalEntityID: "le", JurisdictionID: "IN", CounterpartyID: "cp",
			PaymentReference: ref, GrossPaymentAmount: 1000, WithholdingRatePercent: 10, Currency: "USD",
			EffectiveFrom: "2026-10-01", CreatedBy: "p1"}
	}
	o := mk("PAY-1")
	if err := s.Create(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	o.WithholdingRatePercent = 5
	if err := s.Update(ctx, o); err != nil {
		t.Fatalf("update open obligation: %v", err)
	}
	if _, err := s.Remit(ctx, o.ObligationID, &domain.RemitObligationRequest{RemittanceReference: "R", RemittedBy: "p2"}); err != nil {
		t.Fatalf("remit: %v", err)
	}
	o.WithholdingRatePercent = 0
	if err := s.Update(ctx, o); !errors.Is(err, domain.ErrAlreadyRemitted) {
		t.Fatalf("update remitted: expected ErrAlreadyRemitted, got %v", err)
	}
	got, _ := s.GetByID(ctx, o.ObligationID)
	if got.WithheldAmount != 50 {
		t.Fatalf("remitted amount changed: %v", got.WithheldAmount)
	}

	c := mk("PAY-2")
	if err := s.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Cancel(ctx, c.ObligationID, &domain.CancelObligationRequest{Reason: "x"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := s.Update(ctx, c); !errors.Is(err, domain.ErrAlreadyCancelled) {
		t.Fatalf("update cancelled: expected ErrAlreadyCancelled, got %v", err)
	}
}
