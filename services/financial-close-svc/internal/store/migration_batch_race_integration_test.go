//go:build integration

// Real-Postgres regression test for SPEC_DEVIATIONS A9's ACC-17 slice: two
// concurrent CreateMigrationAccountingBatch calls for the same
// (legal_entity_id, fiscal_period, source_system_name) both pass the
// handler's own existence check and race the migration_batches UNIQUE
// constraint. The loser must be told who won (domain.ErrMigrationBatchRaceLost),
// not answered a generic store failure.
package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestPgStore_CreateMigrationBatch_ConcurrentCreateLoses_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)

	winner := loadedMigrationBatch("race system")
	if err := s.CreateMigrationBatch(ctx, winner); err != nil {
		t.Fatalf("first create (the winner): %v", err)
	}

	loser := &domain.MigrationBatch{
		CurrencyCode: storeTestCurrency,
		BatchID:      uuid.New().String(), LegalEntityID: winner.LegalEntityID, FiscalPeriod: winner.FiscalPeriod,
		SourceSystemName: winner.SourceSystemName, SourceExtractHash: "sha256:y", ExpectedRowCount: 1,
		Status: domain.MigrationBatchStatusLoaded, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-2",
		Entries: []domain.MigrationCrosswalkEntry{{EntryID: uuid.New().String(), SourceReferenceID: "R2", TargetAccountCode: "1000-Cash"}},
	}
	err := s.CreateMigrationBatch(ctx, loser)
	if err == nil {
		t.Fatal("a second create for the same legal entity, period and source system must not also succeed")
	}
	if err != domain.ErrMigrationBatchRaceLost {
		t.Fatalf("want domain.ErrMigrationBatchRaceLost, got %v", err)
	}

	existing, err := s.GetMigrationBatchBySourceSystem(ctx, winner.LegalEntityID, winner.FiscalPeriod, winner.SourceSystemName)
	if err != nil {
		t.Fatalf("GetMigrationBatchBySourceSystem: %v", err)
	}
	if existing.BatchID != winner.BatchID {
		t.Fatalf("the loser's handler must be able to find the actual winner: got %q, want %q", existing.BatchID, winner.BatchID)
	}
}
