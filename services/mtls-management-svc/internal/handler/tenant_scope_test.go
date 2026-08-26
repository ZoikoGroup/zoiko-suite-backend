package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tenant-scope tests for mtls-management-svc (tracker row 8m).
//
// The store here is correctly scoped on every read and mutation. The single
// defect was a getTenant helper returning the literal "default-tenant" when
// X-Tenant-ID was absent — so the correct store faithfully enforced a
// synthetic tenant shared by every header-less caller.
//
// On this service that shared bucket was a control plane over the
// platform's mutual-TLS trust: RevokeCert and RotateCert (via
// ReplaceCertMaterial) are scoped the same way as the reads, so header-less
// callers could revoke or re-issue each other's certificates. Revoking a
// certificate breaks the service-to-service authentication depending on it.
//
// No RLS dimension: this service has no migrations and no Postgres, so the
// in-memory store plus this handler is the entire boundary.

func provisionCert(t *testing.T, r http.Handler, tenantID, serviceName string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"service_name":    serviceName,
		"common_name":     serviceName + ".zoiko.internal",
		"rotation_days":   90,
		"auto_rotate":     false,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision cert for %s: expected 201, got %d — %s", tenantID, w.Code, w.Body.String())
	}
	var res struct {
		Certificate struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
		} `json:"certificate"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode provisioned cert: %v", err)
	}
	// The store now stamps attribution from the verified tenant rather than
	// accepting the parameter and dropping it, so this must hold.
	if res.Certificate.TenantID != tenantID {
		t.Fatalf("certificate attributed to %q, expected %q", res.Certificate.TenantID, tenantID)
	}
	return res.Certificate.ID
}

func TestMissingTenantHeader_Refused(t *testing.T) {
	r := newRouter(t)

	provisionBody, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1", "service_name": "svc-a",
		"common_name": "svc-a.zoiko.internal", "rotation_days": 90,
	})
	policyBody, _ := json.Marshal(map[string]string{
		"policy_name": "p1", "source_service": "svc-a", "target_service": "svc-b",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"provision certificate", http.MethodPost, "/v1/mtls/certificates", provisionBody},
		{"list certificates", http.MethodGet, "/v1/mtls/certificates", nil},
		{"get certificate", http.MethodGet, "/v1/mtls/certificates/some-id", nil},
		{"rotate certificate", http.MethodPost, "/v1/mtls/certificates/some-id/rotate", nil},
		{"revoke certificate", http.MethodDelete, "/v1/mtls/certificates/some-id", nil},
		{"create policy", http.MethodPost, "/v1/mtls/policies", policyBody},
		{"list policies", http.MethodGet, "/v1/mtls/policies", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Deliberately NO X-Tenant-ID.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-ID, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("default-tenant")) {
				t.Fatalf("response must never mention a fabricated default tenant: %s", w.Body.String())
			}
		})
	}
}

// TestForeignTenant_CannotRevokeCertificate is the one that matters most on
// this service. It asserts the refusal AND that the certificate is still
// ACTIVE afterwards — a 404 that had already revoked would pass a
// status-code-only check while having broken another tenant's mTLS.
func TestForeignTenant_CannotRevokeCertificate(t *testing.T) {
	r := newRouter(t)

	certID := provisionCert(t, r, "tenant-a", "svc-a")

	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/v1/mtls/certificates/" + certID},
		{http.MethodPost, "/v1/mtls/certificates/" + certID + "/rotate"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Tenant-ID", "tenant-b")
		req.Header.Set("X-Principal-Id", "principal-test-01")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s as tenant-b: expected 404, got %d — %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates/"+certID, nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant-a must still read its own certificate, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Status      string `json:"status"`
		ServiceName string `json:"service_name"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ServiceName != "svc-a" {
		t.Fatalf("unexpected certificate returned to tenant-a: %+v", got)
	}
	if got.Status == "REVOKED" {
		t.Fatalf("ISOLATION FAILURE: tenant-b's refused request still revoked tenant-a's certificate")
	}
}

// TestForeignTenant_CannotListOthersCertificates covers the read side. The
// certificate record carries the PEM and fingerprint, so an unscoped list
// discloses the platform's internal service topology.
func TestForeignTenant_CannotListOthersCertificates(t *testing.T) {
	r := newRouter(t)

	provisionCert(t, r, "tenant-a", "svc-a")
	provisionCert(t, r, "tenant-b", "svc-b")

	// legal_entity_id is now mandatory: it is the authorization scope for a
	// listing, and an optional scope parameter is a scope that disables
	// itself. Both tenants provision under le-1, so this asks for the SAME
	// legal entity tenant-a used — which is what makes the isolation
	// assertion below meaningful rather than incidental.
	req := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("svc-a")) {
		t.Fatalf("ISOLATION FAILURE: tenant-b's certificate list contained tenant-a's service: %s", w.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected tenant-b to see exactly its own 1 certificate, got %d", resp.Count)
	}
}

// TestTwoHeaderlessCallersDoNotShareANamespace is the direct regression test
// for the old behaviour: both callers became "default-tenant" and the second
// could see and revoke the first's certificate.
func TestTwoHeaderlessCallersDoNotShareANamespace(t *testing.T) {
	r := newRouter(t)

	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1", "service_name": "shared-bucket-svc",
		"common_name": "shared.zoiko.internal", "rotation_days": 90,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("header-less provision must be refused, got %d: %s", w.Code, w.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusUnauthorized {
		t.Fatalf("header-less list must be refused, got %d: %s", listW.Code, listW.Body.String())
	}
	if bytes.Contains(listW.Body.Bytes(), []byte("shared-bucket-svc")) {
		t.Fatalf("ISOLATION FAILURE: a header-less caller saw another header-less caller's certificate: %s", listW.Body.String())
	}
}
