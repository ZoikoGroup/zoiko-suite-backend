package handler

import (
	"bytes"
	"encoding/json"
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
// one bucket. On a tax-authority interface that bucket holds filing
// amounts and the authority's acknowledgements of them.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r, _ := setupTestRouter()

	ifaceBody := []byte(`{"legal_entity_id":"le-101","jurisdiction":"GB","authority_name":"HMRC MTD UK","protocol":"REST/OAuth2"}`)
	filingBody := []byte(`{"interface_id":"some-id","tax_period":"2026-Q1","filing_type":"VAT","tax_amount":1234.56}`)

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create interface", "POST", "/v1/tax-authority/interfaces", ifaceBody},
		{"list interfaces", "GET", "/v1/tax-authority/interfaces", nil},
		{"get interface", "GET", "/v1/tax-authority/interfaces/some-id", nil},
		{"submit filing", "POST", "/v1/tax-authority/filings", filingBody},
		{"list filings", "GET", "/v1/tax-authority/filings", nil},
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

// TestListSubmissions_NoFilter_DoesNotLeakOtherTenants is the HTTP-layer
// counterpart to the store test, and covers this service's most sensitive
// leak: ListSubmissions' only predicate was on interface_id, so omitting
// it — the shorter, easier request — returned every tenant's tax filings,
// tax_amount included.
func TestListSubmissions_NoFilter_DoesNotLeakOtherTenants(t *testing.T) {
	r, _ := setupTestRouter()

	// Tenant A registers an interface and files against it.
	ifaceBody := []byte(`{"legal_entity_id":"le-a","jurisdiction":"GB","authority_name":"HMRC MTD UK","protocol":"REST/OAuth2"}`)
	req := httptest.NewRequest("POST", "/v1/tax-authority/interfaces", bytes.NewReader(ifaceBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create interface: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		InterfaceID string `json:"interface_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created interface: %v (%s)", err, w.Body.String())
	}

	filingBody := []byte(`{"interface_id":"` + created.InterfaceID + `","tax_period":"2026-Q1","filing_type":"VAT","tax_amount":987654.32}`)
	fReq := httptest.NewRequest("POST", "/v1/tax-authority/filings", bytes.NewReader(filingBody))
	fReq.Header.Set("Content-Type", "application/json")
	fReq.Header.Set("X-Tenant-Id", "tenant-a")
	fReq.Header.Set("X-Principal-Id", "principal-01")
	fW := httptest.NewRecorder()
	r.ServeHTTP(fW, fReq)
	// 202 only, now that this endpoint records the filing honestly (PENDING,
	// no fabricated ack) instead of claiming a transmission that never
	// happened. This assertion used to tolerate 201 too, which is how the
	// fabricated-success defect passed unnoticed.
	if fW.Code != http.StatusAccepted {
		t.Fatalf("submit filing: expected 202, got %d: %s", fW.Code, fW.Body.String())
	}

	// Tenant B lists filings with no filter at all — the exact call that leaked.
	listReq := httptest.NewRequest("GET", "/v1/tax-authority/filings", nil)
	listReq.Header.Set("X-Tenant-Id", "tenant-b")
	listReq.Header.Set("X-Principal-Id", "principal-01")
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listW.Code, listW.Body.String())
	}
	for _, needle := range []string{"tenant-a", "987654.32"} {
		if bytes.Contains(listW.Body.Bytes(), []byte(needle)) {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered filing list contained %q: %s", needle, listW.Body.String())
		}
	}
}

// TestGetInterfaceByID_OtherTenant_NotFound covers the direct-read path:
// the lookup was `WHERE interface_id = $1` alone, so any caller holding
// or guessing an interface_id could read which tax authorities another
// tenant files with.
func TestGetInterfaceByID_OtherTenant_NotFound(t *testing.T) {
	r, _ := setupTestRouter()

	ifaceBody := []byte(`{"legal_entity_id":"le-a","jurisdiction":"GB","authority_name":"HMRC MTD UK","protocol":"REST/OAuth2"}`)
	req := httptest.NewRequest("POST", "/v1/tax-authority/interfaces", bytes.NewReader(ifaceBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create interface: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		InterfaceID string `json:"interface_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created interface: %v (%s)", err, w.Body.String())
	}

	getReq := httptest.NewRequest("GET", "/v1/tax-authority/interfaces/"+created.InterfaceID, nil)
	getReq.Header.Set("X-Tenant-Id", "tenant-b")
	getReq.Header.Set("X-Principal-Id", "principal-01")
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's interface, got %d: %s", getW.Code, getW.Body.String())
	}
	if bytes.Contains(getW.Body.Bytes(), []byte("HMRC")) {
		t.Fatalf("ISOLATION FAILURE: tenant B saw tenant A's authority_name: %s", getW.Body.String())
	}
}
