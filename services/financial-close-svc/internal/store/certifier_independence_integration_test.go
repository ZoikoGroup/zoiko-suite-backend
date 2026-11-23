//go:build integration

// Real-Postgres regression test for SPEC_DEVIATIONS A6 (ACC-14): the close
// certifier must be independent from the preparer who most recently brought
// the period into the state being closed from. GetLatestTransitionPrincipal
// is the store-level primitive the handler's independence check relies on.
package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestPgStore_GetLatestTransitionPrincipal_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	fp := openFiscalPeriod(t, ctx, s, "certifier independence")

	// No transition into CLOSE_REVIEW yet: no known preparer.
	principal, err := s.GetLatestTransitionPrincipal(ctx, fp.FiscalPeriodID, domain.PeriodCloseReview)
	if err != nil {
		t.Fatalf("GetLatestTransitionPrincipal before any transition: %v", err)
	}
	if principal != "" {
		t.Fatalf("no CLOSE_REVIEW transition yet, want empty preparer, got %q", principal)
	}

	if _, _, err := s.StartSoftCloseTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "soft-closed", "period.soft_closed", "corr-1", "accountant-1", uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("soft-close: %v", err)
	}
	if _, _, err := s.EnterCloseReviewTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "controller-1", At: now,
	}, "close-review-started", "period.close_review_started", "corr-2", "controller-1", uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("close-review: %v", err)
	}

	preparer, err := s.GetLatestTransitionPrincipal(ctx, fp.FiscalPeriodID, domain.PeriodCloseReview)
	if err != nil {
		t.Fatalf("GetLatestTransitionPrincipal after close-review: %v", err)
	}
	if preparer != "controller-1" {
		t.Fatalf("preparer = %q, want controller-1", preparer)
	}

	// A different toState (SOFT_CLOSE) still answers its own preparer, not the
	// CLOSE_REVIEW one.
	softCloser, err := s.GetLatestTransitionPrincipal(ctx, fp.FiscalPeriodID, domain.PeriodSoftClose)
	if err != nil {
		t.Fatalf("GetLatestTransitionPrincipal(SOFT_CLOSE): %v", err)
	}
	if softCloser != "accountant-1" {
		t.Fatalf("soft-close preparer = %q, want accountant-1", softCloser)
	}
}
