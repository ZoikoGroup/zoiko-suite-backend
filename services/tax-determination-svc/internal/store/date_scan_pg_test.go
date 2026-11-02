package store_test

import (
	"context"
	"testing"

	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/middleware"
	"zoiko.io/tax-determination-svc/internal/store"
)

// DATE columns (effective_from, effective_to, supply_date) are read into
// strings; pgx cannot scan a binary DATE into *string, so every get/list used
// to fail. Regression for the ::text casts in determinationColumns.
func TestPgStore_GetAndList_ScanDateColumns(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-date-scan")
	s := store.NewPgStore(pool)

	supply := "2026-10-15"
	to := "2026-12-31"
	d := &domain.TaxDetermination{
		TransactionID: "txn-1", SourceModule: "INVOICE", LegalEntityID: "le-1", JurisdictionID: "GB",
		TaxCategory: "VAT", GrossAmount: 100, TaxableAmount: 100, Currency: "GBP",
		EffectiveFrom: "2026-10-01", EffectiveTo: &to, EvaluatedBy: "p1",
		SellerPartyID: "s", BuyerPartyID: "b", SupplyJurisdictionID: "GB", SupplyDate: &supply,
		PlaceOfSupplyBasis: domain.PlaceOfSupplyCallerAsserted, ProductClassification: "SAAS",
		SupplyKind: domain.SupplyKindServices, SupplyType: domain.SupplyTypeB2C,
	}
	if err := s.CreateDetermination(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetDetermination(ctx, d.DeterminationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EffectiveFrom != "2026-10-01" || got.EffectiveTo == nil || *got.EffectiveTo != to ||
		got.SupplyDate == nil || *got.SupplyDate != supply {
		t.Fatalf("dates not round-tripped: from=%q to=%v supply=%v", got.EffectiveFrom, got.EffectiveTo, got.SupplyDate)
	}
	list, err := s.ListDeterminations(ctx, "txn-1", "", "")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v (n=%d)", err, len(list))
	}
	if _, err := s.OverrideDetermination(ctx, d.DeterminationID, &domain.OverrideTaxRequest{OverriddenTaxAmount: 1, Reason: "r"}); err != nil {
		t.Fatalf("override: %v", err)
	}
}
