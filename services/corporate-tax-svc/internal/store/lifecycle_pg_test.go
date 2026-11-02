package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/middleware"
	"zoiko.io/corporate-tax-svc/internal/store"
)

func newReturn() *domain.TaxReturn {
	return &domain.TaxReturn{LegalEntityID: "le-1", JurisdictionID: "GB", FiscalYear: 2026,
		AccountingPeriodStart: "2025-04-01", AccountingPeriodEnd: "2026-03-31", GrossRevenue: 100,
		TaxRatePercent: 25, Currency: "GBP", EffectiveFrom: "2026-04-01", CreatedBy: "p1"}
}

// Assess used to accept a DRAFT return, skipping submission (and its evidence
// gate) entirely.
func TestPgStore_Assess_RequiresSubmitted(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-assess")
	s := store.NewPgStore(pool)
	r := newReturn()
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := &domain.AssessTaxReturnRequest{AssessedTaxAmount: 25, AssessmentReference: "REF"}
	if _, err := s.Assess(ctx, r.ReturnID, req); !errors.Is(err, domain.ErrInvalidStatusTransition) {
		t.Fatalf("assess DRAFT: expected ErrInvalidStatusTransition, got %v", err)
	}
	if _, err := s.Submit(ctx, r.ReturnID, "p2"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	got, err := s.Assess(ctx, r.ReturnID, req)
	if err != nil || got.Status != domain.StatusAssessed {
		t.Fatalf("assess SUBMITTED: %v status=%v", err, got)
	}
	if _, err := s.Assess(ctx, r.ReturnID, req); !errors.Is(err, domain.ErrInvalidStatusTransition) {
		t.Fatalf("re-assess: expected ErrInvalidStatusTransition, got %v", err)
	}
}

// A second return for the same entity/jurisdiction/fiscal year used to be a
// raw 23505 and a 500.
func TestPgStore_Create_DuplicateIsErrDuplicateReturn(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-dup")
	s := store.NewPgStore(pool)
	if err := s.Create(ctx, newReturn()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, newReturn()); !errors.Is(err, domain.ErrDuplicateReturn) {
		t.Fatalf("expected ErrDuplicateReturn, got %v", err)
	}
}
