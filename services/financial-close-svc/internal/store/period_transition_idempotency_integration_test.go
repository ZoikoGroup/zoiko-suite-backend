//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A11's ACC-14 slice:
// StartSoftCloseTransition and EnterCloseReviewTransition now claim an
// Idempotency-Key (the mirror of the ACC-07/08/09/10/17 tests in the
// sibling *_idempotency_integration_test.go files). HardClose/Reclose/
// Reopen are deliberately not part of this claim — see the SPEC_DEVIATIONS
// note on why.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/store"
)

func openFiscalPeriod(t *testing.T, ctx context.Context, s *store.PgStore, name string) *domain.FiscalPeriod {
	t.Helper()
	fp := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.New().String(), LegalEntityID: "le-1", PeriodName: name,
		PeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
		CloseStatus: domain.PeriodOpen,
	}
	if _, err := s.CreateFiscalPeriod(ctx, fp); err != nil {
		t.Fatalf("CreateFiscalPeriod: %v", err)
	}
	return fp
}

func TestPgStore_StartSoftCloseTransition_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	fp := openFiscalPeriod(t, ctx, s, "soft-close replay")

	key := uuid.NewString()
	updated, replay, err := s.StartSoftCloseTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "soft-closed", "period.soft_closed", "corr-1", "accountant-1", key, testRequestHash)
	if err != nil || replay {
		t.Fatalf("first soft-close: replay=%v err=%v", replay, err)
	}
	if updated.CloseStatus != domain.PeriodSoftClose {
		t.Fatalf("status after soft-close = %q", updated.CloseStatus)
	}

	retried, replay, err := s.StartSoftCloseTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "soft-closed", "period.soft_closed", "corr-1", "accountant-1", key, testRequestHash)
	if err != nil || !replay {
		t.Fatalf("retried soft-close: want replay, got replay=%v err=%v", replay, err)
	}
	if retried.CloseStatus != domain.PeriodSoftClose {
		t.Fatalf("replay status = %q, want SOFT_CLOSE", retried.CloseStatus)
	}

	// A genuinely new attempt (a different key) against the now-SOFT_CLOSE
	// period must still be refused, and still report the current period.
	again, replay, err := s.StartSoftCloseTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "soft-closed", "period.soft_closed", "corr-2", "accountant-1", uuid.NewString(), testRequestHash)
	if err == nil || replay {
		t.Fatalf("new soft-close attempt on an already-SOFT_CLOSE period: want a refusal, got replay=%v err=%v", replay, err)
	}
	if again == nil || again.CloseStatus != domain.PeriodSoftClose {
		t.Fatalf("refusal must still report the current period, got %+v", again)
	}
}

func TestPgStore_EnterCloseReviewTransition_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	fp := openFiscalPeriod(t, ctx, s, "close-review replay")
	if _, _, err := s.StartSoftCloseTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "soft-closed", "period.soft_closed", "corr-1", "accountant-1", uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("soft-close: %v", err)
	}

	key := uuid.NewString()
	updated, replay, err := s.EnterCloseReviewTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "close-review-started", "period.close_review_started", "corr-2", "accountant-1", key, testRequestHash)
	if err != nil || replay {
		t.Fatalf("first close-review: replay=%v err=%v", replay, err)
	}
	if updated.CloseStatus != domain.PeriodCloseReview {
		t.Fatalf("status after close-review = %q", updated.CloseStatus)
	}

	retried, replay, err := s.EnterCloseReviewTransition(ctx, fp.FiscalPeriodID, domain.PeriodUpdate{
		PrincipalID: "accountant-1", At: now,
	}, "close-review-started", "period.close_review_started", "corr-2", "accountant-1", key, testRequestHash)
	if err != nil || !replay {
		t.Fatalf("retried close-review: want replay, got replay=%v err=%v", replay, err)
	}
	if retried.CloseStatus != domain.PeriodCloseReview {
		t.Fatalf("replay status = %q, want CLOSE_REVIEW", retried.CloseStatus)
	}
}
