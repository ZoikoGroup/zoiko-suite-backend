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

// ── FXExposureSnapshot (Wave 15) ────────────────────────────────────────────

func sampleFXExposureCalc(stale bool) domain.FXExposureCalculation {
	return domain.FXExposureCalculation{
		RateUsed: 1.08, RateAsOf: time.Now().UTC(), RateVersion: "rate-1", HasStaleComponent: stale,
		Buckets: []domain.FXExposureBucket{
			{MaturityBucket: "0-30D", Category: "RECEIVABLE", ExposureCurrencyAmount: 1000, FunctionalCurrencyAmount: 1080},
			{MaturityBucket: "31-90D", Category: "PAYABLE", ExposureCurrencyAmount: -400, FunctionalCurrencyAmount: -432},
		},
		GrossExposureAmount: 1512, NetExposureAmount: 648, AsOfTimestamp: time.Now().UTC(),
	}
}

// TestPgStore_FXExposureSnapshot_CalculateThenPublish is the real
// end-to-end proof of Wave 15's Calculated->Published lifecycle,
// mirroring Wave 14's cash-position proof exactly.
func TestPgStore_FXExposureSnapshot_CalculateThenPublish(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1", CorrelationID: "corr-fx-1",
	}, sampleFXExposureCalc(false))
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if snap.Status != domain.FXExposureCalculated || snap.EffectiveStatus != domain.FXExposureCalculated {
		t.Fatalf("expected CALCULATED, got status=%s effective=%s", snap.Status, snap.EffectiveStatus)
	}
	if snap.NettingScope != "SINGLE_ENTITY:"+legalEntityID {
		t.Fatalf("expected netting_scope to be recorded, got %q", snap.NettingScope)
	}
	if len(snap.Buckets) != 2 {
		t.Fatalf("expected 2 buckets to round-trip, got %d", len(snap.Buckets))
	}

	published, err := s.PublishFXExposureSnapshot(ctx, domain.PublishFXExposureParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "approver-1"})
	if err != nil || published.Status != domain.FXExposurePublished {
		t.Fatalf("publish: %+v err=%v", published, err)
	}

	// Negative control: publishing again must fail.
	if _, err := s.PublishFXExposureSnapshot(ctx, domain.PublishFXExposureParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "approver-2"}); err != domain.ErrInvalidFXExposureTransition {
		t.Fatalf("expected ErrInvalidFXExposureTransition re-publishing, got %v", err)
	}
}

// TestPgStore_FXExposureSnapshot_StaleCannotPublish proves a snapshot
// calculated from a stale rate cannot be published as current.
func TestPgStore_FXExposureSnapshot_StaleCannotPublish(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, sampleFXExposureCalc(true))
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if _, err := s.PublishFXExposureSnapshot(ctx, domain.PublishFXExposureParams{TenantID: tenantID, SnapshotID: snap.SnapshotID, ActorPrincipalID: "approver-1"}); err != domain.ErrFXExposureStaleCannotPublish {
		t.Fatalf("expected ErrFXExposureStaleCannotPublish, got %v", err)
	}
}

// TestPgStore_FXExposureSnapshot_RefreshSupersedesPrior proves the
// refresh/supersede chain, mirroring Wave 14's cash-position proof.
func TestPgStore_FXExposureSnapshot_RefreshSupersedesPrior(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, sampleFXExposureCalc(false))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := s.PublishFXExposureSnapshot(ctx, domain.PublishFXExposureParams{TenantID: tenantID, SnapshotID: first.SnapshotID, ActorPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("publish first: %v", err)
	}

	second, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, sampleFXExposureCalc(false))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	superseded, err := s.SupersedeFXExposureSnapshot(ctx, domain.SupersedeFXExposureParams{TenantID: tenantID, SnapshotID: first.SnapshotID, NewSnapshotID: second.SnapshotID})
	if err != nil || superseded.Status != domain.FXExposureSuperseded {
		t.Fatalf("supersede: %+v err=%v", superseded, err)
	}
	if superseded.SupersededBy == nil || *superseded.SupersededBy != second.SnapshotID {
		t.Fatalf("expected superseded_by to point at the new snapshot, got %+v", superseded.SupersededBy)
	}

	latest, err := s.GetLatestFXExposure(ctx, tenantID, legalEntityID, "EUR", "USD")
	if err != nil || latest == nil || latest.SnapshotID != second.SnapshotID {
		t.Fatalf("expected the latest fx exposure to be the second snapshot, got %+v err=%v", latest, err)
	}
}

// TestPgStore_FXExposureSnapshot_SupersededIsImmutable is the
// negative-controlled proof of migration 000009's own
// reject_fx_exposure_snapshot_mutation trigger.
func TestPgStore_FXExposureSnapshot_SupersededIsImmutable(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, sampleFXExposureCalc(false))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, sampleFXExposureCalc(false))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := s.SupersedeFXExposureSnapshot(ctx, domain.SupersedeFXExposureParams{TenantID: tenantID, SnapshotID: first.SnapshotID, NewSnapshotID: second.SnapshotID}); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	if _, err := testPool.Exec(ctx, `UPDATE fx_exposure_snapshots SET net_exposure_amount = 999999 WHERE snapshot_id = $1`, first.SnapshotID); err == nil {
		t.Fatal("expected the trigger to refuse mutating a SUPERSEDED snapshot")
	}

	if _, err := testPool.Exec(ctx, `ALTER TABLE fx_exposure_snapshots DISABLE TRIGGER trg_reject_fx_exposure_snapshot_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE fx_exposure_snapshots SET net_exposure_amount = 999999 WHERE snapshot_id = $1`, first.SnapshotID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE fx_exposure_snapshots ENABLE TRIGGER trg_reject_fx_exposure_snapshot_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE fx_exposure_snapshots SET net_exposure_amount = 1 WHERE snapshot_id = $1`, first.SnapshotID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestPgStore_FXExposureSnapshot_AsOfReconstructsHistory mirrors Wave
// 14's as-of proof.
func TestPgStore_FXExposureSnapshot_AsOfReconstructsHistory(t *testing.T) {
	cleanTables(t)
	s := testStore
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, sampleFXExposureCalc(false))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	var midpoint time.Time
	if err := testPool.QueryRow(ctx, `SELECT created_at FROM fx_exposure_snapshots WHERE snapshot_id = $1`, first.SnapshotID).Scan(&midpoint); err != nil {
		t.Fatalf("read midpoint: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	second := sampleFXExposureCalc(false)
	second.NetExposureAmount = 9999
	if _, err := s.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	asOf, err := s.GetFXExposureAsOf(ctx, tenantID, legalEntityID, "EUR", "USD", midpoint.Add(1*time.Millisecond))
	if err != nil {
		t.Fatalf("GetFXExposureAsOf: %v", err)
	}
	if asOf == nil || asOf.NetExposureAmount != 648 {
		t.Fatalf("expected the FIRST snapshot's net_exposure_amount=648 at the midpoint, got %+v", asOf)
	}

	current, err := s.GetFXExposureAsOf(ctx, tenantID, legalEntityID, "EUR", "USD", time.Now().UTC())
	if err != nil || current == nil || current.NetExposureAmount != 9999 {
		t.Fatalf("expected the SECOND (latest) snapshot's net_exposure_amount=9999 as of now, got %+v err=%v", current, err)
	}
}
