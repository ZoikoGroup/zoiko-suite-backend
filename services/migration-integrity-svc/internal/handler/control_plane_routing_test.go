package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The program/extract routes are wired through the same notConfigured()
// guard as promote/crosswalk/certificate: on an instance where
// WithControlPlane was never called (newRouter in this package's tests
// uses a plain MemoryStore, exactly that case), every one of them must
// answer 501, not 404 (routing mistake) or a panic (nil h.cp dereference).
func TestControlPlaneRoutes_AnswerNotImplementedWhenUnconfigured(t *testing.T) {
	router := newRouter(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create program", http.MethodPost, "/v1/migrations/programs", `{"program_id":"P1","source_estate":"ERP","target_scope":"gl","owner_principal_id":"o1","cutover_strategy":"PHASED"}`},
		{"get program", http.MethodGet, "/v1/migrations/programs/some-id", ""},
		{"update program status", http.MethodPost, "/v1/migrations/programs/some-id/status", `{"status":"IN_PROGRESS"}`},
		{"register extract", http.MethodPost, "/v1/migrations/extracts", `{"legal_entity_id":"le-1","source_watermark":"w","generated_at":"2026-01-01T00:00:00Z","manifest_hash":"h","record_count":1}`},
		{"get extract", http.MethodGet, "/v1/migrations/extracts/some-id", ""},
		{"list extract versions", http.MethodGet, "/v1/migrations/extracts?extract_id=x&legal_entity_id=le-1", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, bytes.NewBufferString(c.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Principal-Id", "principal-1")
			withEnvelope(req)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("%s %s: expected 501, got %d: %s", c.method, c.path, rec.Code, rec.Body.String())
			}
		})
	}
}
