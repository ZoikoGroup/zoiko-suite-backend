package envelope

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteViolation_CarriesTheOrgTypedErrorCode pins ORG §3's stable typed
// error on the envelope refusal: alongside the human-readable
// envelope_incomplete and the per-field violations, a caller must see
// error_code CONTEXT_INVALID.
//
// The field was added on 28 Sep 2026 (b9e244fc) and lost again when this
// package was regenerated from the contract template, which does not carry it.
// The live audit caught that (section 15: "an envelope refusal is
// CONTEXT_INVALID"); this test keeps the next regeneration from silently
// dropping it again.
func TestWriteViolation_CarriesTheOrgTypedErrorCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeViolation(rec, &ValidationError{
		Service: "tenant-entity-registry-svc",
		Violations: []Violation{
			{Field: "tenant_id", Header: HeaderTenantID, Reason: "mandatory: tenant authority and isolation boundary"},
		},
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refusal status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := rec.Header().Get("X-Envelope-Contract"); got != "violated" {
		t.Fatalf("X-Envelope-Contract = %q, want violated", got)
	}

	var body struct {
		Error      string      `json:"error"`
		ErrorCode  string      `json:"error_code"`
		Violations []Violation `json:"violations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error != "envelope_incomplete" {
		t.Fatalf("error = %q, want envelope_incomplete", body.Error)
	}
	if body.ErrorCode != "CONTEXT_INVALID" {
		t.Fatalf("error_code = %q, want CONTEXT_INVALID", body.ErrorCode)
	}
	if len(body.Violations) != 1 {
		t.Fatalf("violations = %v, want exactly the one supplied", body.Violations)
	}
}
