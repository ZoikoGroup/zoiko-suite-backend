package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/financial-close-svc/internal/domain"
)

// A9 (SPEC_DEVIATIONS): two concurrent creates for the same legal entity,
// period and source system race the store's UNIQUE constraint; the loser
// must be told who won (200, the existing batch), not a 503.
func TestCreateMigrationAccountingBatch_ConcurrentCreateLoses_ReturnsWinner(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"1000-Cash": "ACTIVE"}}
	r := newRouter(s, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		CurrencyCode:  testCurrency,
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 1, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Entries: []domain.MigrationCrosswalkEntry{{SourceReferenceID: "SRC-1", TargetAccountCode: "1000-Cash", DebitAmount: 500}},
	}

	// The winner is already on record (as a concurrent request would have
	// left it), and the store reports this request's own insert lost the
	// race — exactly what the real store's mapPgError does for a unique
	// violation on migration_batches.
	winner := &domain.MigrationBatch{BatchID: "winner-batch", LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", Status: domain.MigrationBatchStatusLoaded}
	s.migrationBatches[winner.BatchID] = winner
	s.createMigrationErr = domain.ErrMigrationBatchRaceLost

	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("the race loser must be answered the winner: %d %s", rr.Code, rr.Body.String())
	}
	var got domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.BatchID != winner.BatchID {
		t.Fatalf("expected the winning batch %q, got %q", winner.BatchID, got.BatchID)
	}
}

func TestValidateOpeningBalances_AROpenItemAlreadyExistsInHistory_QuarantinesBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		accountStatuses:    map[string]string{"1000-Cash": "ACTIVE", "1200-AR": "ACTIVE"},
		existingARInvoices: map[string]bool{"cust-1|INV-100": true}, // already a real invoice in AR's own history
	}
	r := newRouter(s, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		CurrencyCode:  testCurrency,
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "INV-100", TargetAccountCode: "1200-AR", DebitAmount: 500,
				SourceReferenceType: strPtr(domain.MigrationCrosswalkTypeAROpenItem), PartyID: strPtr("cust-1")},
			{SourceReferenceID: "SRC-2", TargetAccountCode: "1000-Cash", CreditAmount: 500},
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", validateRR.Code, validateRR.Body.String())
	}
	stored, _ := s.GetMigrationBatch(context.Background(), batch.BatchID)
	if stored.Status != domain.MigrationBatchStatusQuarantined {
		t.Fatalf("expected QUARANTINED, got %q", stored.Status)
	}
}

func TestValidateOpeningBalances_APOpenItemAlreadyExistsInHistory_QuarantinesBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		accountStatuses:    map[string]string{"1000-Cash": "ACTIVE", "2100-AP": "ACTIVE"},
		existingAPInvoices: map[string]bool{"vendor-1|BILL-200": true},
	}
	r := newRouter(s, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		CurrencyCode:  testCurrency,
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "SRC-1", TargetAccountCode: "1000-Cash", DebitAmount: 500},
			{SourceReferenceID: "BILL-200", TargetAccountCode: "2100-AP", CreditAmount: 500,
				SourceReferenceType: strPtr(domain.MigrationCrosswalkTypeAPOpenItem), PartyID: strPtr("vendor-1")},
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", validateRR.Code, validateRR.Body.String())
	}
}

func TestValidateOpeningBalances_AROpenItemNotInHistory_ValidationSucceeds(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{
		accountStatuses: map[string]string{"1000-Cash": "ACTIVE", "1200-AR": "ACTIVE"},
		// existingARInvoices left empty — nothing exists in AR's history yet.
	}
	r := newRouter(s, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		CurrencyCode:  testCurrency,
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Entries: []domain.MigrationCrosswalkEntry{
			{SourceReferenceID: "INV-100", TargetAccountCode: "1200-AR", DebitAmount: 500,
				SourceReferenceType: strPtr(domain.MigrationCrosswalkTypeAROpenItem), PartyID: strPtr("cust-1")},
			{SourceReferenceID: "SRC-2", TargetAccountCode: "1000-Cash", CreditAmount: 500},
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", validateRR.Code, validateRR.Body.String())
	}
}

func TestValidateOpeningBalances_OpenItemMissingPartyID_QuarantinesBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"1000-Cash": "ACTIVE", "1200-AR": "ACTIVE"}}
	r := newRouter(s, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		CurrencyCode:  testCurrency,
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x",
		ExpectedRowCount: 2, ExpectedTotalDebits: 500, ExpectedTotalCredits: 500,
		Entries: []domain.MigrationCrosswalkEntry{
			// Flagged as an AR open item but no party_id given.
			{SourceReferenceID: "INV-100", TargetAccountCode: "1200-AR", DebitAmount: 500,
				SourceReferenceType: strPtr(domain.MigrationCrosswalkTypeAROpenItem)},
			{SourceReferenceID: "SRC-2", TargetAccountCode: "1000-Cash", CreditAmount: 500},
		},
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	_ = json.NewDecoder(rr.Body).Decode(&batch)

	validateRR := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", validateRR.Code, validateRR.Body.String())
	}
}
