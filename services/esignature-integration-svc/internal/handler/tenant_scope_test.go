package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/esignature-integration-svc/internal/domain"
)

// createEnvelopeAs POSTs an envelope as the given tenant and returns its id.
func createEnvelopeAs(t *testing.T, r http.Handler, tenantID, legalEntityID string) string {
	t.Helper()

	body, _ := json.Marshal(domain.CreateEnvelopeRequest{
		LegalEntityID: legalEntityID,
		Provider:      "DOCUSIGN",
		DocumentTitle: "Board Resolution 2026-01",
		SignerEmail:   "signer@example.com",
		SignerName:    "A Signer",
	})
	req := httptest.NewRequest("POST", "/v1/esignature/envelopes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create envelope as %s: expected 201, got %d: %s", tenantID, w.Code, w.Body.String())
	}

	var created domain.SignatureEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created envelope: %v", err)
	}
	if created.EnvelopeID == "" {
		t.Fatalf("created envelope has no envelope_id: %s", w.Body.String())
	}
	return created.EnvelopeID
}

// TestMissingTenantHeader_Refused proves the fabricated-identity fix.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r, _ := setupTestRouter(t)

	body, _ := json.Marshal(domain.CreateEnvelopeRequest{
		LegalEntityID: "le-101", Provider: "DOCUSIGN",
		DocumentTitle: "Doc", SignerEmail: "s@example.com", SignerName: "S",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create envelope", "POST", "/v1/esignature/envelopes", body},
		{"list envelopes", "GET", "/v1/esignature/envelopes", nil},
		{"get envelope", "GET", "/v1/esignature/envelopes/some-id", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Deliberately NO X-Tenant-Id.
			req.Header.Set("X-Principal-Id", "principal-01")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("default-tenant")) {
				t.Fatalf("response must never mention a fabricated default tenant: %s", w.Body.String())
			}
		})
	}
}

// TestGetEnvelopeByID_ForeignTenant_NotFound proves the read-scoping fix.
// The query was `WHERE envelope_id = $1` alone, exposing another tenant's
// document_title plus signer_email and signer_name — personal data about
// the signer.
func TestGetEnvelopeByID_ForeignTenant_NotFound(t *testing.T) {
	r, _ := setupTestRouter(t)

	envID := createEnvelopeAs(t, r, "tenant-a", "le-a")

	req := httptest.NewRequest("GET", "/v1/esignature/envelopes/"+envID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's envelope — expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Sanity: tenant A can still read its own.
	own := httptest.NewRequest("GET", "/v1/esignature/envelopes/"+envID, nil)
	own.Header.Set("X-Tenant-Id", "tenant-a")
	own.Header.Set("X-Principal-Id", "principal-01")

	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, own)
	if ownW.Code != http.StatusOK {
		t.Fatalf("tenant A must still read its own envelope, got %d: %s", ownW.Code, ownW.Body.String())
	}
}

// TestUpdateEnvelopeStatus_ForeignTenant_Refused is the most important
// test in this file. UpdateEnvelopeStatus was an unscoped WRITE
// (`WHERE envelope_id = $4` alone), so any caller holding another tenant's
// envelope_id could mark that tenant's document SIGNED or COMPLETED and
// set its external_ref.
//
// Doc 03 §16.5 makes this service the governed external execution path for
// contracts, board resolutions and legal artifacts, so a forged status
// transition is a legal-integrity problem, not merely a data one. The test
// asserts both that the write is refused AND that tenant A's status is
// genuinely unchanged afterwards — a refusal that still mutated would be
// the worse outcome.
func TestUpdateEnvelopeStatus_ForeignTenant_Refused(t *testing.T) {
	r, _ := setupTestRouter(t)

	envID := createEnvelopeAs(t, r, "tenant-a", "le-a")

	body, _ := json.Marshal(domain.UpdateStatusRequest{
		Status:      "COMPLETED",
		ExternalRef: "forged-ref",
	})
	req := httptest.NewRequest("POST", "/v1/esignature/envelopes/"+envID+"/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("ISOLATION FAILURE: tenant B changed the signature status of tenant A's envelope: %s", w.Body.String())
	}

	// And tenant A's envelope must be genuinely untouched.
	check := httptest.NewRequest("GET", "/v1/esignature/envelopes/"+envID, nil)
	check.Header.Set("X-Tenant-Id", "tenant-a")
	check.Header.Set("X-Principal-Id", "principal-01")

	checkW := httptest.NewRecorder()
	r.ServeHTTP(checkW, check)
	if checkW.Code != http.StatusOK {
		t.Fatalf("tenant A must still read its own envelope, got %d", checkW.Code)
	}

	var after domain.SignatureEnvelope
	if err := json.Unmarshal(checkW.Body.Bytes(), &after); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if after.Status == "COMPLETED" || after.ExternalRef == "forged-ref" {
		t.Fatalf("ISOLATION FAILURE: tenant A's envelope was mutated by tenant B: status=%q external_ref=%q", after.Status, after.ExternalRef)
	}
}

// TestListEnvelopes_OmittedFilter_DoesNotLeakOtherTenants proves the
// self-disabling-filter fix: the query matched when the legal_entity_id
// parameter was the empty string OR equalled the column, so omitting it
// returned EVERY tenant's envelopes.
func TestListEnvelopes_OmittedFilter_DoesNotLeakOtherTenants(t *testing.T) {
	r, _ := setupTestRouter(t)

	createEnvelopeAs(t, r, "tenant-a", "le-a")
	createEnvelopeAs(t, r, "tenant-b", "le-b")

	req := httptest.NewRequest("GET", "/v1/esignature/envelopes", nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Accept either a bare array or an enveloped shape, so this test does
	// not silently pass if the response shape changes.
	var envelopes []domain.SignatureEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelopes); err != nil {
		var wrapped struct {
			Envelopes []domain.SignatureEnvelope `json:"envelopes"`
			Total     int                        `json:"total"`
		}
		if err2 := json.Unmarshal(w.Body.Bytes(), &wrapped); err2 != nil {
			t.Fatalf("unmarshal list as array (%v) and as envelope (%v): body=%s", err, err2, w.Body.String())
		}
		envelopes = wrapped.Envelopes
	}

	for _, e := range envelopes {
		if e.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's envelope %s", e.TenantID, e.EnvelopeID)
		}
	}
	if len(envelopes) != 1 {
		t.Fatalf("expected exactly tenant B's own 1 envelope, got %d: %+v", len(envelopes), envelopes)
	}
}
