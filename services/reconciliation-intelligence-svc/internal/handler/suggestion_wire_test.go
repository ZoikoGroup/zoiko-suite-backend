package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// analyzeAndDecodeRaw posts a run-sourced analysis and returns the first
// suggestion as a raw JSON object, so the wire names can be checked without
// going through the Go field names.
func analyzeAndDecodeRaw(t *testing.T, router http.Handler, tenantID, legalEntityID, runID string) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(analyzeBody(legalEntityID, runID)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-01")
	req = withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("analyze: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		JobID          string           `json:"id"`
		UnmatchedItems []map[string]any `json:"unmatched_items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode analyze: %v", err)
	}
	if len(body.UnmatchedItems) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(body.UnmatchedItems))
	}
	item := body.UnmatchedItems[0]
	item["_job_id"] = body.JobID
	return item
}

func applySuggestion(t *testing.T, router http.Handler, tenantID, jobID, itemID, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost,
		"/v1/reconciliations/"+jobID+"/resolutions/"+itemID+"/apply", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-02")
	req = withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// A9: the response uses the canonical data-model names and, for one
// release, keeps the pre-rename aliases in the same object. Both carry the
// same value.
func TestSuggestionWire_CanonicalNamesAndLegacyAliases(t *testing.T) {
	router := setupTestRouter(t)
	item := analyzeAndDecodeRaw(t, router, "tenant-wire-1", "le-1", runSmall)

	want := map[string]string{
		"suggestion_status": "RECOMMENDED",
		"resolution_status": "RECOMMENDED",
		"source_record_a":   "REF-sl-small",
		"transaction_ref_a": "REF-sl-small",
		"legal_entity_id":   "le-1",
	}
	for key, value := range want {
		if got, _ := item[key].(string); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

// A9: existing callers (the Postman collection is one) still send
// resolution_status. It must be accepted as an alias for one release.
func TestSuggestionWire_LegacyResolutionStatusAccepted(t *testing.T) {
	router := setupTestRouter(t)
	item := analyzeAndDecodeRaw(t, router, "tenant-wire-2", "le-1", runSmall)
	jobID, _ := item["_job_id"].(string)
	itemID, _ := item["id"].(string)

	rec := applySuggestion(t, router, "tenant-wire-2", jobID, itemID,
		`{"resolution_status":"REJECTED","resolution_notes":"legacy caller"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy apply: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var applied map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if applied["suggestion_status"] != "REJECTED" || applied["resolution_status"] != "REJECTED" {
		t.Fatalf("status aliases disagree or are missing: %v", applied)
	}
}

// A9: the canonical name is the one new callers use.
func TestSuggestionWire_CanonicalSuggestionStatusAccepted(t *testing.T) {
	router := setupTestRouter(t)
	item := analyzeAndDecodeRaw(t, router, "tenant-wire-3", "le-1", runSmall)
	jobID, _ := item["_job_id"].(string)
	itemID, _ := item["id"].(string)

	rec := applySuggestion(t, router, "tenant-wire-3", jobID, itemID,
		`{"suggestion_status":"REJECTED","resolution_notes":"new caller"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("canonical apply: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Sending both names with different values is ambiguous: refuse it rather
// than silently choosing one during the alias release.
func TestSuggestionWire_ConflictingAliasesRefused(t *testing.T) {
	router := setupTestRouter(t)
	item := analyzeAndDecodeRaw(t, router, "tenant-wire-4", "le-1", runSmall)
	jobID, _ := item["_job_id"].(string)
	itemID, _ := item["id"].(string)

	rec := applySuggestion(t, router, "tenant-wire-4", jobID, itemID,
		`{"suggestion_status":"APPROVED","resolution_status":"REJECTED"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("conflicting aliases: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
