package acceptance

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
)

// Doc 05 §08–09: an unsupported locale falls back to the server default, so
// a Spanish-browser visitor asking in English still gets the approved answer
// instead of an abstention caused by locale filtering.
func TestUnsupportedLocaleFallsBackToDefault(t *testing.T) {
	x := publicKBServer(t)
	w := x.req(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0","locale":"es-ES"}`, map[string]string{"Idempotency-Key": "locale-fallback-1"})
	var s contract.SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != 201 {
		t.Fatalf("session: %d %s", w.Code, w.Body.String())
	}
	e := envelope(t, x.turn(s, "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", "What is ZoikoSuite?"))
	if e.Status != contract.Answered {
		t.Fatalf("status %s, want ANSWERED: %q", e.Status, e.Text)
	}
}
