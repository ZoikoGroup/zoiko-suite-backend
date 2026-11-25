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
	if err := s.Create(ctx, r, store.EventMeta{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := &domain.AssessTaxReturnRequest{AssessedTaxAmount: 25, AssessmentReference: "REF"}
	if _, err := s.Assess(ctx, r.ReturnID, req, "p3", store.EventMeta{}); !errors.Is(err, domain.ErrInvalidStatusTransition) {
		t.Fatalf("assess DRAFT: expected ErrInvalidStatusTransition, got %v", err)
	}
	if _, err := s.Submit(ctx, r.ReturnID, "p2", store.EventMeta{}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	got, err := s.Assess(ctx, r.ReturnID, req, "p3", store.EventMeta{})
	if err != nil || got.Status != domain.StatusAssessed {
		t.Fatalf("assess SUBMITTED: %v status=%v", err, got)
	}
	if _, err := s.Assess(ctx, r.ReturnID, req, "p3", store.EventMeta{}); !errors.Is(err, domain.ErrInvalidStatusTransition) {
		t.Fatalf("re-assess: expected ErrInvalidStatusTransition, got %v", err)
	}
}

// A second return for the same entity/jurisdiction/fiscal year used to be a
// raw 23505 and a 500.
func TestPgStore_Create_DuplicateIsErrDuplicateReturn(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-dup")
	s := store.NewPgStore(pool)
	if err := s.Create(ctx, newReturn(), store.EventMeta{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, newReturn(), store.EventMeta{}); !errors.Is(err, domain.ErrDuplicateReturn) {
		t.Fatalf("expected ErrDuplicateReturn, got %v", err)
	}
}

// Segregation of duties, enforced under the row lock: the submitter of a
// return may not record its assessment.
func TestPgStore_Assess_BySubmitterIsErrSegregationOfDuties(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-sod")
	s := store.NewPgStore(pool)
	r := newReturn()
	if err := s.Create(ctx, r, store.EventMeta{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Submit(ctx, r.ReturnID, "p2", store.EventMeta{}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	req := &domain.AssessTaxReturnRequest{AssessedTaxAmount: 25, AssessmentReference: "REF"}
	if _, err := s.Assess(ctx, r.ReturnID, req, "p2", store.EventMeta{}); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("assess by submitter: expected ErrSegregationOfDuties, got %v", err)
	}
	got, err := s.GetByID(ctx, r.ReturnID)
	if err != nil || got.Status != domain.StatusSubmitted {
		t.Fatalf("refused assessment must leave the return SUBMITTED: %v %v", err, got)
	}
}
