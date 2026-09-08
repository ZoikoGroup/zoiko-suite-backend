package store_test

// ACC-17's own closure of "Open AR included both in history and opening
// state" adds source_reference_type/party_id columns (migration 000013)
// to migration_crosswalk_entries — this file proves they round-trip
// through the real database and that the CHECK constraint on
// source_reference_type is real, not just documentation. Skips (not
// fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func strPtr(s string) *string { return &s }

func TestPgStore_ACC17_CrosswalkEntryOpenItemFields_RoundTrip_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	batch := &domain.MigrationBatch{
		BatchID: uuid.New().String(), TenantID: tenantID, LegalEntityID: "le-1",
		FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Status: domain.MigrationBatchStatusLoaded, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
		Entries: []domain.MigrationCrosswalkEntry{
			{
				EntryID: uuid.New().String(), SourceReferenceID: "INV-100", TargetAccountCode: "1200-AR",
				DebitAmount: 500, SourceReferenceType: strPtr(domain.MigrationCrosswalkTypeAROpenItem), PartyID: strPtr("cust-1"),
			},
			{
				EntryID: uuid.New().String(), SourceReferenceID: "SRC-2", TargetAccountCode: "1000-Cash",
				CreditAmount: 500,
			},
		},
	}
	if err := s.CreateMigrationBatch(ctx, batch); err != nil {
		t.Fatalf("CreateMigrationBatch: %v", err)
	}

	got, err := s.GetMigrationBatch(ctx, batch.BatchID)
	if err != nil {
		t.Fatalf("GetMigrationBatch: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got.Entries))
	}
	var arEntry *domain.MigrationCrosswalkEntry
	for i := range got.Entries {
		if got.Entries[i].SourceReferenceID == "INV-100" {
			arEntry = &got.Entries[i]
		}
	}
	if arEntry == nil {
		t.Fatal("expected to find the INV-100 entry")
	}
	if arEntry.SourceReferenceType == nil || *arEntry.SourceReferenceType != domain.MigrationCrosswalkTypeAROpenItem {
		t.Fatalf("expected source_reference_type AR_OPEN_ITEM, got %+v", arEntry.SourceReferenceType)
	}
	if arEntry.PartyID == nil || *arEntry.PartyID != "cust-1" {
		t.Fatalf("expected party_id cust-1, got %+v", arEntry.PartyID)
	}

	// The ordinary GL balance entry carries neither field.
	var plainEntry *domain.MigrationCrosswalkEntry
	for i := range got.Entries {
		if got.Entries[i].SourceReferenceID == "SRC-2" {
			plainEntry = &got.Entries[i]
		}
	}
	if plainEntry == nil {
		t.Fatal("expected to find the SRC-2 entry")
	}
	if plainEntry.SourceReferenceType != nil || plainEntry.PartyID != nil {
		t.Fatalf("expected nil source_reference_type/party_id for a plain GL line, got %+v / %+v", plainEntry.SourceReferenceType, plainEntry.PartyID)
	}
}

// TestPgStore_ACC17_InvalidSourceReferenceType_RejectedByCheckConstraint
// proves migration 000013's CHECK constraint is real, not just
// documentation.
func TestPgStore_ACC17_InvalidSourceReferenceType_RejectedByCheckConstraint(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	batch := &domain.MigrationBatch{
		BatchID: uuid.New().String(), TenantID: tenantID, LegalEntityID: "le-1",
		FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 1, ExpectedTotalDebits: 500, ExpectedTotalCredits: 0,
		Status: domain.MigrationBatchStatusLoaded, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
		Entries: []domain.MigrationCrosswalkEntry{
			{
				EntryID: uuid.New().String(), SourceReferenceID: "INV-100", TargetAccountCode: "1200-AR",
				DebitAmount: 500, SourceReferenceType: strPtr("NOT_A_REAL_TYPE"), PartyID: strPtr("cust-1"),
			},
		},
	}
	if err := s.CreateMigrationBatch(ctx, batch); err == nil {
		t.Fatal("expected the CHECK constraint to reject an unrecognized source_reference_type value")
	}
}
