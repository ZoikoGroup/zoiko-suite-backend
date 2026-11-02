package store_test

import (
	"context"
	"testing"

	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/middleware"
	"zoiko.io/compliance-status-svc/internal/store"
)

// Re-evaluating the same entity/jurisdiction/domain upserts the existing row,
// but used to return a freshly generated status_id that was never stored, so
// a GET of the returned id was a 404.
func TestPgStore_Evaluate_ReEvaluateReturnsPersistedStatusID(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-cs")
	s := store.NewPgStore(pool)
	first := &domain.ComplianceHealth{LegalEntityID: "le", JurisdictionID: "GB", DomainName: "TAX",
		TotalObligations: 10, FulfilledObligations: 9, CreatedBy: "p1"}
	if err := s.Evaluate(ctx, first); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	second := &domain.ComplianceHealth{LegalEntityID: "le", JurisdictionID: "GB", DomainName: "TAX",
		TotalObligations: 10, FulfilledObligations: 5, OverdueObligations: 2, CreatedBy: "p1"}
	if err := s.Evaluate(ctx, second); err != nil {
		t.Fatalf("re-evaluate: %v", err)
	}
	if second.StatusID != first.StatusID {
		t.Fatalf("re-evaluate returned %s, persisted row is %s", second.StatusID, first.StatusID)
	}
	got, err := s.GetByID(ctx, second.StatusID)
	if err != nil {
		t.Fatalf("get returned id: %v", err)
	}
	if got.OverallStatus != domain.StatusNonCompliant {
		t.Fatalf("expected NON_COMPLIANT, got %s", got.OverallStatus)
	}
}
