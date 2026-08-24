package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
)

// createBridgeAs POSTs a bridge as the given tenant and returns its id.
func createBridgeAs(t *testing.T, r http.Handler, tenantID, legalEntityID string) string {
	t.Helper()

	body, _ := json.Marshal(domain.CreateBridgeRequest{
		LegalEntityID: legalEntityID,
		BridgeName:    "Test Bridge",
		Protocol:      "REST",
		EndpointURL:   "https://erp.example.internal/api",
		AuthType:      "OAUTH2",
	})
	req := httptest.NewRequest("POST", "/v1/bridges/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create bridge as %s: expected 201, got %d: %s", tenantID, w.Code, w.Body.String())
	}

	var created domain.ApiBridge
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created bridge: %v", err)
	}
	if created.BridgeID == "" {
		t.Fatalf("created bridge has no bridge_id: %s", w.Body.String())
	}
	return created.BridgeID
}

// TestMissingTenantHeader_Refused proves the fabricated-identity fix.
//
// The middleware used to substitute the literal string "default-tenant"
// when X-Tenant-Id was absent, so a header-less request SUCCEEDED under a
// tenant that does not exist — and every header-less caller shared that
// one bucket. Omitting the header was the easier request to make, which
// made the insecure path the path of least resistance.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r, _ := setupTestRouter(t)

	body, _ := json.Marshal(domain.CreateBridgeRequest{
		LegalEntityID: "le-101",
		BridgeName:    "Test Bridge",
		Protocol:      "REST",
		EndpointURL:   "https://erp.example.internal/api",
		AuthType:      "OAUTH2",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create bridge", "POST", "/v1/bridges/", body},
		{"list bridges", "GET", "/v1/bridges/", nil},
		{"get bridge", "GET", "/v1/bridges/some-id", nil},
		{"list logs", "GET", "/v1/bridges/some-id/logs", nil},
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

// TestGetBridgeByID_ForeignTenant_NotFound proves the read-scoping fix.
// GetBridgeByID's query was `WHERE bridge_id = $1` alone, so any caller
// holding another tenant's bridge_id could read its integration
// configuration — endpoint_url and auth_type included.
//
// 404 rather than 403 is deliberate: a distinct forbidden status would
// confirm that another tenant's bridge_id exists.
func TestGetBridgeByID_ForeignTenant_NotFound(t *testing.T) {
	r, _ := setupTestRouter(t)

	bridgeID := createBridgeAs(t, r, "tenant-a", "le-a")

	req := httptest.NewRequest("GET", "/v1/bridges/"+bridgeID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's bridge — expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Sanity: tenant A can still read its own.
	own := httptest.NewRequest("GET", "/v1/bridges/"+bridgeID, nil)
	own.Header.Set("X-Tenant-Id", "tenant-a")
	own.Header.Set("X-Principal-Id", "principal-01")

	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, own)
	if ownW.Code != http.StatusOK {
		t.Fatalf("tenant A must still read its own bridge, got %d: %s", ownW.Code, ownW.Body.String())
	}
}

// TestListBridges_OmittedFilter_DoesNotLeakOtherTenants proves the
// self-disabling-filter fix.
//
// The query matched when the legal_entity_id parameter was the empty
// string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// EVERY tenant's bridges. The legal-entity dimension is legitimately
// optional; the tenant dimension never is.
func TestListBridges_OmittedFilter_DoesNotLeakOtherTenants(t *testing.T) {
	r, _ := setupTestRouter(t)

	createBridgeAs(t, r, "tenant-a", "le-a")
	createBridgeAs(t, r, "tenant-b", "le-b")

	req := httptest.NewRequest("GET", "/v1/bridges/", nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The route returns an envelope and repeats the rows under both
	// "bridges" and "connections" (registered at two paths for
	// compatibility). Check both, so a leak through either key is caught.
	var got struct {
		Bridges     []domain.ApiBridge `json:"bridges"`
		Connections []domain.ApiBridge `json:"connections"`
		Total       int                `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal list: %v (body=%s)", err, w.Body.String())
	}

	for key, list := range map[string][]domain.ApiBridge{
		"bridges":     got.Bridges,
		"connections": got.Connections,
	} {
		for _, b := range list {
			if b.TenantID != "tenant-b" {
				t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's bridge %s under %q", b.TenantID, b.BridgeID, key)
			}
		}
		if len(list) != 1 {
			t.Fatalf("%s: expected exactly tenant B's own 1 bridge, got %d: %+v", key, len(list), list)
		}
	}
	if got.Total != 1 {
		t.Fatalf("expected total=1 for tenant B, got %d", got.Total)
	}
}
