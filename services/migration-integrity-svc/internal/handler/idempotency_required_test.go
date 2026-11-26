package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/migration-integrity-svc/internal/domain"
	svcenvelope "zoiko.io/migration-integrity-svc/internal/envelope"
)

// TestWriteWithoutIdempotencyKey_RefusedInEveryEnvelopeMode: the envelope
// admits a key-less write when ZS_ENVELOPE_ENFORCEMENT=observe and the store
// records nothing for an empty key. The service itself refuses it with 400
// in every mode, and nothing is written.
func TestWriteWithoutIdempotencyKey_RefusedInEveryEnvelopeMode(t *testing.T) {
	for _, mode := range []string{"observe", "write_strict", "strict"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(svcenvelope.EnvVarMode, mode)
			router := newRouter(t) // envelope mode is read when the router is built

			body, _ := json.Marshal(domain.ValidateMigrationRequest{
				LegalEntityID: "LE-IDEM", MigrationName: "no key", SourceSystem: "LEGACY_ERP",
				TargetService: "ledger-svc", ExceptionOwnerID: "owner-1", RequiredFields: []string{"amount"},
				Records: []domain.MigrationRecord{{Ref: "R-1", Fields: map[string]string{"amount": "1.00"}}},
			})
			for _, tc := range []struct {
				method, path string
				body         []byte
			}{
				{http.MethodPost, "/v1/migrations/validate", body},
				{http.MethodDelete, "/v1/migrations/00000000-0000-0000-0000-000000000001", nil},
			} {
				req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				withEnvelope(req)
				req.Header.Del("Idempotency-Key")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("%s %s without key: expected 400, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
				}
				if mode == "observe" {
					var problem map[string]any
					_ = json.Unmarshal(rec.Body.Bytes(), &problem)
					if problem["code"] != "idempotency-key-required" {
						t.Fatalf("observe: expected idempotency-key-required, got %v", problem["code"])
					}
				}
			}

			// Nothing was written, and reads still need no key.
			req := httptest.NewRequest(http.MethodGet, "/v1/migrations/", nil)
			withEnvelope(req)
			req.Header.Del("Idempotency-Key")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("list without key: %d %s", rec.Code, rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("no key")) {
				t.Fatalf("a refused write must not create a job: %s", rec.Body.String())
			}
		})
	}
}
