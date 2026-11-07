package idempotency

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/store"
)

// A refusal from the replay middleware is RFC 9457 problem+json (ZS-API-001
// §12), and a recorded refusal replays as one.
func TestRefusalsAndReplayedRefusalsAreProblems(t *testing.T) {
	w := httptest.NewRecorder()
	writeErr(w, httptest.NewRequest(http.MethodPost, "/v1/admin/roles", nil), http.StatusConflict,
		"idempotency_mismatch", "this Idempotency-Key was already used for a different request")
	var p map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != http.StatusConflict || w.Header().Get("Content-Type") != "application/problem+json" ||
		p["code"] != "idempotency_mismatch" || p["error_class"] != "IDEMPOTENCY_MISMATCH" {
		t.Fatalf("want a 409 problem, got %d %q %v", w.Code, w.Header().Get("Content-Type"), p)
	}

	w = httptest.NewRecorder()
	replay(w, &store.IdempotencyRecord{ResponseStatus: http.StatusForbidden, ResponseBody: w.Body.Bytes()})
	if w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("a replayed 403 is served as %q", w.Header().Get("Content-Type"))
	}
	w = httptest.NewRecorder()
	replay(w, &store.IdempotencyRecord{ResponseStatus: http.StatusCreated, ResponseBody: []byte(`{"role_id":"r-1"}`)})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("a replayed 201 is %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}
