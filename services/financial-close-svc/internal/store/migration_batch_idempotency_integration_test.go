//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A11's ACC-17 slice:
// ApproveMigrationBatch and CertifyMigrationBatch now claim an
// Idempotency-Key (the mirror of the ACC-07/08/09/10 tests in the sibling
// *_idempotency_integration_test.go files). Validate is deliberately not
// part of this claim (see the SPEC_DEVIATIONS note on why); Commit already
// has its own idempotency via the handler's RECONCILED/CERTIFIED/POSTED
// status switch.
package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

func loadedMigrationBatch(sourceSystem string) *domain.MigrationBatch {
	return &domain.MigrationBatch{
		CurrencyCode: storeTestCurrency,
		BatchID:      uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		SourceSystemName: sourceSystem, SourceExtractHash: "sha256:x", ExpectedRowCount: 1,
		Status: domain.MigrationBatchStatusLoaded, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
		Entries: []domain.MigrationCrosswalkEntry{{EntryID: uuid.New().String(), SourceReferenceID: "R1", TargetAccountCode: "1000-Cash"}},
	}
}

func TestPgStore_ApproveMigrationBatch_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	batch := loadedMigrationBatch("approve replay")
	if err := s.CreateMigrationBatch(ctx, batch); err != nil {
		t.Fatalf("CreateMigrationBatch: %v", err)
	}
	if err := s.MarkMigrationBatchValidated(ctx, batch.BatchID, now); err != nil {
		t.Fatalf("MarkMigrationBatchValidated: %v", err)
	}
	batch.Status = domain.MigrationBatchStatusValidated

	key := uuid.NewString()
	if replay, err := s.ApproveMigrationBatch(ctx, batch, "approver-1", key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first approve: replay=%v err=%v", replay, err)
	}
	if batch.Status != domain.MigrationBatchStatusApproved {
		t.Fatalf("status after approve = %q", batch.Status)
	}

	retryBatch := &domain.MigrationBatch{BatchID: batch.BatchID}
	replay, err := s.ApproveMigrationBatch(ctx, retryBatch, "approver-1", key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried approve: want replay, got replay=%v err=%v", replay, err)
	}
	if retryBatch.Status != domain.MigrationBatchStatusApproved {
		t.Fatalf("replay status = %q, want APPROVED", retryBatch.Status)
	}
	if len(retryBatch.Entries) != 1 {
		t.Fatalf("replay must also carry the crosswalk entries, got %+v", retryBatch.Entries)
	}
}

func TestPgStore_CertifyMigrationBatch_RetryUnderSameKeyReplays_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	batch := loadedMigrationBatch("certify replay")
	if err := s.CreateMigrationBatch(ctx, batch); err != nil {
		t.Fatalf("CreateMigrationBatch: %v", err)
	}
	if err := s.MarkMigrationBatchValidated(ctx, batch.BatchID, now); err != nil {
		t.Fatalf("MarkMigrationBatchValidated: %v", err)
	}
	batch.Status = domain.MigrationBatchStatusValidated
	if _, err := s.ApproveMigrationBatch(ctx, batch, "approver-1", uuid.NewString(), testRequestHash, now); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.MarkMigrationBatchPosted(ctx, batch.BatchID, "journal-1", now); err != nil {
		t.Fatalf("MarkMigrationBatchPosted: %v", err)
	}
	if err := s.MarkMigrationBatchReconciled(ctx, batch.BatchID, now); err != nil {
		t.Fatalf("MarkMigrationBatchReconciled: %v", err)
	}

	key := uuid.NewString()
	if replay, err := s.CertifyMigrationBatch(ctx, batch, "certifier-1", "cutover signed", key, testRequestHash, now); err != nil || replay {
		t.Fatalf("first certify: replay=%v err=%v", replay, err)
	}
	if batch.Status != domain.MigrationBatchStatusCertified {
		t.Fatalf("status after certify = %q", batch.Status)
	}

	retryBatch := &domain.MigrationBatch{BatchID: batch.BatchID}
	replay, err := s.CertifyMigrationBatch(ctx, retryBatch, "certifier-1", "cutover signed", key, testRequestHash, now)
	if err != nil || !replay {
		t.Fatalf("retried certify: want replay, got replay=%v err=%v", replay, err)
	}
	if retryBatch.Status != domain.MigrationBatchStatusCertified {
		t.Fatalf("replay status = %q, want CERTIFIED", retryBatch.Status)
	}
}
