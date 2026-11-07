package apicontract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// legacyEnvelope writes exactly what the vendored §4 middleware writes.
func legacyEnvelope(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Envelope-Contract", "violated")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"envelope_incomplete","detail":"missing request_id","service":"svc","violations":[{"field":"request_id","header":"X-Request-Id","reason":"missing"}]}`))
	})
}

func TestEnvelopeRefusalsRendersProblem(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{http.StatusBadRequest, "envelope_incomplete"}, {http.StatusUnauthorized, "identity_missing"}} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/things", nil)
		req.Header.Set(HeaderRequestID, "req-1")
		EnvelopeRefusals(legacyEnvelope(tc.status)).ServeHTTP(rr, req)

		if rr.Code != tc.status || rr.Header().Get("Content-Type") != ContentTypeProblem {
			t.Fatalf("got %d %q, want %d problem", rr.Code, rr.Header().Get("Content-Type"), tc.status)
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if body["code"] != tc.code || body["detail"] != "missing request_id" || body["request_id"] != "req-1" {
			t.Fatalf("problem members wrong: %v", body)
		}
		if body["error"] != "envelope_incomplete" || body["violations"] == nil || body["service"] != "svc" {
			t.Fatalf("legacy members must be kept: %v", body)
		}
	}
}

func TestEnvelopeRefusalsPassesAdmittedAndOtherResponsesThrough(t *testing.T) {
	admitted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Envelope-Contract", "violated")
		_, _ = w.Write([]byte(`[]`))
	})
	rr := httptest.NewRecorder()
	EnvelopeRefusals(admitted).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/things", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "[]" || rr.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("admitted read changed: %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}

	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, "not_found", "")
	})
	rr = httptest.NewRecorder()
	EnvelopeRefusals(other).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/things/x", nil))
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("handler problem changed: %d %v", rr.Code, body)
	}
}
