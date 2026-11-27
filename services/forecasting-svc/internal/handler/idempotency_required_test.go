package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	svcenvelope "zoiko.io/forecasting-svc/internal/envelope"
)

// TestWriteWithoutIdempotencyKey_RefusedInEveryEnvelopeMode: the envelope
// admits a key-less write when ZS_ENVELOPE_ENFORCEMENT=observe, and the store
// treats an empty key as "no replay protection". The service itself must
// refuse it with 400 before the store is reached, in every mode.
func TestWriteWithoutIdempotencyKey_RefusedInEveryEnvelopeMode(t *testing.T) {
	for _, mode := range []string{"observe", "write_strict", "strict"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(svcenvelope.EnvVarMode, mode)
			st := newFakeStore()
			router := newRouter(st, allowAll())
			for _, tc := range []struct{ method, path, body string }{
				{http.MethodPost, "/v1/forecasts/generate", validForecast},
				{http.MethodDelete, "/v1/forecasts/00000000-0000-0000-0000-000000000001", ""},
			} {
				w := send(router, tc.method, tc.path, tc.body, func(r *http.Request) { r.Header.Del("Idempotency-Key") })
				if w.Code != http.StatusBadRequest {
					t.Fatalf("%s %s without key: %d %s", tc.method, tc.path, w.Code, w.Body.String())
				}
				if mode == "observe" {
					var problem map[string]any
					_ = json.Unmarshal(w.Body.Bytes(), &problem)
					if problem["code"] != "idempotency_key_required" {
						t.Fatalf("observe mode: expected idempotency_key_required, got %v", problem["code"])
					}
				}
			}
			if st.creates != 0 {
				t.Fatalf("a key-less write reached the store")
			}
			// Reads are unaffected.
			if w := send(router, http.MethodGet, "/v1/forecasts/00000000-0000-0000-0000-000000000001", "",
				func(r *http.Request) { r.Header.Del("Idempotency-Key") }); w.Code != http.StatusNotFound {
				t.Fatalf("a read must not require Idempotency-Key: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
