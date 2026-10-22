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

func createFXExposure(t *testing.T, ctx context.Context, tenantID, legalEntityID, rateVersion string, rateAsOf time.Time) *domain.FXExposureSnapshot {
	t.Helper()
	snap, err := testStore.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1",
	}, domain.FXExposureCalculation{
		RateUsed: 1.1, RateAsOf: rateAsOf, RateVersion: rateVersion, AsOfTimestamp: time.Now().UTC(),
		Buckets: []domain.FXExposureBucket{{MaturityBucket: "0-30D", Category: "RECEIVABLE", ExposureCurrencyAmount: 100, FunctionalCurrencyAmount: 110}},
	})
	if err != nil {
		t.Fatalf("create fx snapshot: %v", err)
	}
	return snap
}

// A snapshot whose recorded rate has aged past the threshold is stale when
// read and cannot be published; a published one ages into STALE. Before,
// staleness was frozen at calculation, so STALE was unreachable.
func TestPgStore_FXExposure_StalenessDerivedAtRead(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	aged := createFXExposure(t, ctx, tenantID, uuid.New().String(), uuid.New().String(), time.Now().UTC().Add(-48*time.Hour))
	if !aged.HasStaleComponent {
		t.Fatal("a snapshot calculated from a two-day-old rate reads as fresh")
	}
	if _, err := testStore.PublishFXExposureSnapshot(ctx, domain.PublishFXExposureParams{TenantID: tenantID, SnapshotID: aged.SnapshotID, ActorPrincipalID: "publisher-1"}); err != domain.ErrFXExposureStaleCannotPublish {
		t.Fatalf("publishing an aged snapshot: %v, want ErrFXExposureStaleCannotPublish", err)
	}

	// The identity rate of a same-currency snapshot has no recorded rate
	// behind it, so it never goes stale on rate age.
	identity := createFXExposure(t, ctx, tenantID, uuid.New().String(), "", time.Now().UTC().Add(-48*time.Hour))
	if identity.HasStaleComponent {
		t.Fatal("a same-currency snapshot went stale on the age of its identity rate")
	}

	almostStale := createFXExposure(t, ctx, tenantID, uuid.New().String(), uuid.New().String(), time.Now().UTC().Add(-domain.FXRateStalenessThreshold+2*time.Second))
	if _, err := testStore.PublishFXExposureSnapshot(ctx, domain.PublishFXExposureParams{TenantID: tenantID, SnapshotID: almostStale.SnapshotID, ActorPrincipalID: "publisher-1"}); err != nil {
		t.Fatalf("publish while fresh: %v", err)
	}
	time.Sleep(3 * time.Second)
	read, err := testStore.GetFXExposureSnapshot(ctx, tenantID, almostStale.SnapshotID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.EffectiveStatus != "STALE" || read.Status != domain.FXExposurePublished {
		t.Fatalf("published snapshot past the threshold: status=%s effective=%s, want PUBLISHED/STALE", read.Status, read.EffectiveStatus)
	}
}

// Refresh is one transaction and the prior must be for the same legal
// entity and currency pair; the standalone supersede checks the same.
func TestPgStore_FXExposure_RefreshAndSupersedeAreScoped(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entityA, entityB := uuid.New().String(), uuid.New().String()
	priorA := createFXExposure(t, ctx, tenantID, entityA, "", time.Now().UTC())

	_, err := testStore.RefreshFXExposureSnapshot(ctx, domain.RefreshFXExposureParams{
		CalculateFXExposureParams: domain.CalculateFXExposureParams{TenantID: tenantID, LegalEntityID: entityB, ExposureCurrency: "EUR", FunctionalCurrency: "USD", ActorPrincipalID: "analyst-1"},
		PriorSnapshotID:           priorA.SnapshotID,
	}, domain.FXExposureCalculation{AsOfTimestamp: time.Now().UTC()})
	if err != domain.ErrFXExposureScopeMismatch {
		t.Fatalf("refresh for entity B superseding entity A's snapshot: %v, want ErrFXExposureScopeMismatch", err)
	}
	if latestB, err := testStore.GetLatestFXExposure(ctx, tenantID, entityB, "EUR", "USD"); err != nil || latestB != nil {
		t.Fatalf("the refused refresh left a snapshot behind for entity B: %+v err=%v", latestB, err)
	}

	otherEntity := createFXExposure(t, ctx, tenantID, entityB, "", time.Now().UTC())
	if _, err := testStore.SupersedeFXExposureSnapshot(ctx, domain.SupersedeFXExposureParams{TenantID: tenantID, SnapshotID: priorA.SnapshotID, NewSnapshotID: otherEntity.SnapshotID}); err != domain.ErrFXExposureScopeMismatch {
		t.Fatalf("supersede by another entity's snapshot: %v, want ErrFXExposureScopeMismatch", err)
	}

	refreshed, err := testStore.RefreshFXExposureSnapshot(ctx, domain.RefreshFXExposureParams{
		CalculateFXExposureParams: domain.CalculateFXExposureParams{TenantID: tenantID, LegalEntityID: entityA, ExposureCurrency: "EUR", FunctionalCurrency: "USD", ActorPrincipalID: "analyst-1"},
		PriorSnapshotID:           priorA.SnapshotID,
	}, domain.FXExposureCalculation{AsOfTimestamp: time.Now().UTC()})
	if err != nil {
		t.Fatalf("refresh in scope: %v", err)
	}
	a, _ := testStore.GetFXExposureSnapshot(ctx, tenantID, priorA.SnapshotID)
	if a.Status != domain.FXExposureSuperseded || a.SupersededBy == nil || *a.SupersededBy != refreshed.SnapshotID {
		t.Fatalf("prior after refresh: status=%s superseded_by=%v", a.Status, a.SupersededBy)
	}
}
