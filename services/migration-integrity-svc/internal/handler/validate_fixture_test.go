package handler_test

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

// The tests in this package were written before two rules the rewrite added
// to POST /v1/migrations/validate, both part of the documented contract
// (ROUTES.md, e2e_intel_migration.py):
//
//   - control_totals is required (ZS-MIG-001 §17, MIG-INV-15; 400 without it)
//   - a body legal_entity_id must equal X-Legal-Entity-Id (403 otherwise)
//
// Fixtures that are not about either rule go through validateRequest, which
// declares the manifest the batch actually matches and sends the header the
// body names, so each test keeps asserting exactly what it asserted before.

// matchingControlTotals is the manifest a source would declare for records:
// their count and the sum of the amounts that parse as decimals (the same
// rule domain.ReconcileControlTotals applies to the target side).
func matchingControlTotals(records []domain.MigrationRecord) *domain.ControlTotalsDeclaration {
	sum := new(big.Rat)
	for _, rec := range records {
		if v, ok := domain.ParseDecimal(rec.Fields[domain.DefaultControlAmountField]); ok {
			sum.Add(sum, v)
		}
	}
	count := len(records)
	return &domain.ControlTotalsDeclaration{RecordCount: &count, AmountTotal: domain.FormatDecimal(sum)}
}

// validateRequest builds a POST /v1/migrations/validate for req, filling in
// a matching manifest when req declares none and the legal entity header.
func validateRequest(t *testing.T, req domain.ValidateMigrationRequest) *http.Request {
	t.Helper()
	if req.ControlTotals == nil && len(req.Records) > 0 {
		req.ControlTotals = matchingControlTotals(req.Records)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal validate request: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	if req.LegalEntityID != "" {
		r.Header.Set("X-Legal-Entity-Id", req.LegalEntityID)
	}
	r.Header.Set("Content-Type", "application/json")
	return withEnvelope(r)
}
