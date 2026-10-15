package envelope

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMiddlewareRefusal_ProblemDetails verifies that a strict-mode refusal is
// emitted as an RFC 9457 problem (API standard §12) with each unmet §4
// obligation in errors[], located by JSON Pointer.
func TestMiddlewareRefusal_ProblemDetails(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := MiddlewareWithMode(ServicePolicy(), ModeStrict, nil)(next)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant/actor, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	if rr.Header().Get("X-Envelope-Contract") != "violated" {
		t.Error("expected X-Envelope-Contract header to be set on a refusal")
	}

	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "envelope_incomplete" || body["error"] != "envelope_incomplete" {
		t.Errorf("code/error = %v/%v, want envelope_incomplete", body["code"], body["error"])
	}
	if body["type"] != "https://api.zoikosuite.com/problems/authorization-denied" {
		t.Errorf("type = %v, want .../problems/authorization-denied", body["type"])
	}

	errs, ok := body["errors"].([]any)
	if !ok || len(errs) < 5 {
		t.Fatalf("expected the five unconditional-field violations, got %v", body["errors"])
	}
	found := false
	for _, e := range errs {
		em, ok := e.(map[string]any)
		if ok && em["pointer"] == "#/tenant_id" && em["code"] == "violation" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a violation located at #/tenant_id, got %v", errs)
	}
}