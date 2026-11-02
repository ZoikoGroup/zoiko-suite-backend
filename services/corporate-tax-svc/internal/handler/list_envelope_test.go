package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The list answers {corporate_tax_returns, total} like the other tax lists;
// a bare array broke callers that parse the envelope.
func TestListCorporateTaxReturns_Envelope(t *testing.T) {
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	h.List(w, buildRequest(http.MethodGet, "/v1/corporate-tax-returns", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("list must be a JSON object envelope, got %s", w.Body.String())
	}
	if _, ok := body["corporate_tax_returns"]; !ok {
		t.Errorf("missing corporate_tax_returns key: %s", w.Body.String())
	}
	if _, ok := body["total"]; !ok {
		t.Errorf("missing total key: %s", w.Body.String())
	}
}
