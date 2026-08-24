package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMissingTenantHeader_Refused proves the fabricated-identity fix
// (Priority 1b) at the HTTP layer.
//
// The middleware used to substitute the literal string "default-tenant"
// when X-Tenant-Id was absent, so a header-less request SUCCEEDED under a
// tenant that does not exist — and every header-less caller shared that
// one bucket. On an HR connector that bucket holds the address of the
// system of record for a tenant's workforce data.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r, _ := setupTestRouter(t)

	body := []byte(`{"legal_entity_id":"le-101","provider_name":"WORKDAY","api_endpoint":"https://x.workday.example/api"}`)

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create integration", "POST", "/v1/hris/integrations", body},
		{"list integrations", "GET", "/v1/hris/integrations", nil},
		{"get integration", "GET", "/v1/hris/integrations/some-id", nil},
		{"list sync jobs", "GET", "/v1/hris/sync/jobs", nil},
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

// TestListSyncJobs_NoFilter_DoesNotLeakOtherTenants is the HTTP-layer
// counterpart to the store test: ListSyncJobs' only predicate was on
// integration_id, so omitting it returned every tenant's sync history.
func TestListSyncJobs_NoFilter_DoesNotLeakOtherTenants(t *testing.T) {
	r, _ := setupTestRouter(t)

	// Tenant A creates an integration.
	body := []byte(`{"legal_entity_id":"le-a","provider_name":"WORKDAY","api_endpoint":"https://tenant-a.workday.example/api"}`)
	req := httptest.NewRequest("POST", "/v1/hris/integrations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create integration: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Tenant B lists sync jobs with no filter at all.
	listReq := httptest.NewRequest("GET", "/v1/hris/sync/jobs", nil)
	listReq.Header.Set("X-Tenant-Id", "tenant-b")
	listReq.Header.Set("X-Principal-Id", "principal-01")
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listW.Code, listW.Body.String())
	}
	if bytes.Contains(listW.Body.Bytes(), []byte("tenant-a")) {
		t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered sync-job list mentioned tenant-a: %s", listW.Body.String())
	}
}
