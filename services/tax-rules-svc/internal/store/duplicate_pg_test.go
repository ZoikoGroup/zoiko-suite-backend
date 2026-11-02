package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/tax-rules-svc/internal/domain"
	"zoiko.io/tax-rules-svc/internal/middleware"
	"zoiko.io/tax-rules-svc/internal/store"
)

// A second rule with the same (tenant, jurisdiction, rule_code, version) used
// to surface as a raw 23505 and a 500.
func TestPgStore_CreateTaxRule_DuplicateCodeIsErrDuplicateRuleCode(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-dup")
	s := store.NewPgStore(pool)
	mk := func() *domain.TaxRule {
		return &domain.TaxRule{JurisdictionID: "GB", RuleCode: "VAT-STD", Name: "n", Category: domain.CategoryVAT,
			TaxRatePercentage: 20, EffectiveFrom: "2026-01-01", CreatedBy: "p1"}
	}
	if err := s.CreateTaxRule(ctx, mk()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := s.CreateTaxRule(ctx, mk()); !errors.Is(err, domain.ErrDuplicateRuleCode) {
		t.Fatalf("expected ErrDuplicateRuleCode, got %v", err)
	}
}
