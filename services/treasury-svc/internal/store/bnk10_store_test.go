//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// TestPgStore_RecordFXRate_LatestWins proves GetLatestFXRate returns the
// most recently EFFECTIVE rate, not merely the most recently inserted one
// — a correction backdated to an earlier effective_at than the current
// latest must not become "latest."
func TestPgStore_RecordFXRate_LatestWins(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	older, err := s.RecordFXRate(ctx, domain.RecordFXRateParams{
		TenantID: tenantID, CurrencyPair: "EUR/USD", Rate: 1.08,
		EffectiveAt: time.Now().Add(-2 * time.Hour), RecordedByPrincipalID: "fx-admin-1",
	})
	if err != nil {
		t.Fatalf("record older rate: %v", err)
	}
	newer, err := s.RecordFXRate(ctx, domain.RecordFXRateParams{
		TenantID: tenantID, CurrencyPair: "EUR/USD", Rate: 1.09,
		EffectiveAt: time.Now(), RecordedByPrincipalID: "fx-admin-1",
	})
	if err != nil {
		t.Fatalf("record newer rate: %v", err)
	}

	latest, err := s.GetLatestFXRate(ctx, tenantID, "EUR/USD")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.RateID != newer.RateID {
		t.Fatalf("expected the newer rate (id=%s) to be latest, got id=%s", newer.RateID, latest.RateID)
	}
	if latest.RateID == older.RateID {
		t.Fatal("the older rate must not be returned as latest")
	}

	// A different currency pair for this tenant must not have a rate yet.
	if _, err := s.GetLatestFXRate(ctx, tenantID, "GBP/USD"); err != domain.ErrFXRateNotFound {
		t.Fatalf("expected ErrFXRateNotFound for an unrecorded pair, got %v", err)
	}
}

// TestPgStore_FXRate_IsAppendOnly is the negative-controlled proof of
// migration 000005's own reject_fx_rate_mutation trigger.
func TestPgStore_FXRate_IsAppendOnly(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	rate, err := s.RecordFXRate(ctx, domain.RecordFXRateParams{
		TenantID: tenantID, CurrencyPair: "JPY/USD", Rate: 0.0067,
		EffectiveAt: time.Now(), RecordedByPrincipalID: "fx-admin-1",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	if _, err := testPool.Exec(ctx, `UPDATE fx_rates SET rate = 999 WHERE rate_id = $1`, rate.RateID); err == nil {
		t.Fatal("expected the trigger to refuse mutating a recorded FX rate")
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM fx_rates WHERE rate_id = $1`, rate.RateID); err == nil {
		t.Fatal("expected the trigger to refuse deleting a recorded FX rate")
	}

	if _, err := testPool.Exec(ctx, `ALTER TABLE fx_rates DISABLE TRIGGER trg_reject_fx_rate_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE fx_rates SET rate = 999 WHERE rate_id = $1`, rate.RateID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE fx_rates ENABLE TRIGGER trg_reject_fx_rate_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE fx_rates SET rate = 1 WHERE rate_id = $1`, rate.RateID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestPgStore_FXRate_TenantIsolation proves tenant B cannot read tenant
// A's recorded FX rate.
func TestPgStore_FXRate_TenantIsolation(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	if _, err := s.RecordFXRate(ctxA, domain.RecordFXRateParams{
		TenantID: tenantA, CurrencyPair: "CHF/USD", Rate: 1.12, EffectiveAt: time.Now(), RecordedByPrincipalID: "fx-admin-1",
	}); err != nil {
		t.Fatalf("record for tenant A: %v", err)
	}

	if _, err := s.GetLatestFXRate(ctxB, tenantB, "CHF/USD"); err != domain.ErrFXRateNotFound {
		t.Fatalf("tenant isolation failure: expected ErrFXRateNotFound for tenant B, got %v", err)
	}
}
