package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/vat-gst-svc/internal/domain"
	"zoiko.io/vat-gst-svc/internal/middleware"
	"zoiko.io/vat-gst-svc/internal/store"
)

// UpdateVATReturn used to rewrite the figures (and status) of a FILED return.
func TestPgStore_UpdateVATReturn_RefusedOnceFiled(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-vat")
	s := store.NewPgStore(pool)
	r := &domain.VATReturn{LegalEntityID: "le", JurisdictionID: "GB", TaxPeriod: "2026-Q3",
		OutputTaxAmount: 200, InputTaxAmount: 80, Currency: "GBP", EffectiveFrom: "2026-07-01", CreatedBy: "p1"}
	if err := s.CreateVATReturn(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.OutputTaxAmount = 220
	if err := s.UpdateVATReturn(ctx, r); err != nil {
		t.Fatalf("update DRAFT: %v", err)
	}
	if _, err := s.FileVATReturn(ctx, r.ReturnID, "p2"); err != nil {
		t.Fatalf("file: %v", err)
	}
	r.OutputTaxAmount = 1
	if err := s.UpdateVATReturn(ctx, r); !errors.Is(err, domain.ErrAlreadyFiled) {
		t.Fatalf("update FILED: expected ErrAlreadyFiled, got %v", err)
	}
	got, _ := s.GetVATReturn(ctx, r.ReturnID)
	if got.Status != domain.StatusFiled || got.OutputTaxAmount != 220 {
		t.Fatalf("filed return changed: status=%s output=%v", got.Status, got.OutputTaxAmount)
	}
}
