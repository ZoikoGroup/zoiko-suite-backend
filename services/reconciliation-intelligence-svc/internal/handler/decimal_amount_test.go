package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ZS-DATA-001 D06: amounts cross the API as exact decimal strings. A JSON
// number would have to pass through float64 to be decoded, and a value with
// more precision than the NUMERIC(18,4) column would be silently rounded by
// Postgres — both are refused with 400 rather than accepted inexactly.
func TestAnalyzeReconciliation_RejectsInexactAmounts(t *testing.T) {
	router := setupTestRouter(t)

	cases := map[string]string{
		"json number":         `500.00`,
		"five decimal places": `"500.00001"`,
	}
	for name, amount := range cases {
		body := `{"legal_entity_id":"LE-1","job_name":"j","source_system_a":"GENERAL_LEDGER","source_system_b":"BANK_STATEMENTS",` +
			`"transactions_a":[{"ref_id":"TX-1","amount":` + amount + `,"date":"2026-07-01"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", strings.NewReader(body))
		withEnvelope(req)
		req.Header.Set("X-Tenant-ID", "tenant-dec")
		req.Header.Set("X-Principal-Id", "principal-01")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
}
