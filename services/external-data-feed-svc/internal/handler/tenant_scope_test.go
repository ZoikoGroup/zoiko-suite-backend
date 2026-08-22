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
// one bucket, able to read each other's feed data. Omitting the header was
// the easier request to make, which made the insecure path the path of
// least resistance.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r, _ := setupTestRouter(t)

	body := []byte(`{"legal_entity_id":"le-101","provider":"BLOOMBERG","feed_type":"MARKET_DATA","symbol":"AAPL"}`)

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create subscription", "POST", "/v1/external-data-feeds/subscriptions", body},
		{"list subscriptions", "GET", "/v1/external-data-feeds/subscriptions", nil},
		{"get subscription", "GET", "/v1/external-data-feeds/subscriptions/some-id", nil},
		{"list events", "GET", "/v1/external-data-feeds/events", nil},
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

// TestListEvents_NoFeedFilter_DoesNotLeakOtherTenants is the HTTP-layer
// counterpart to the store test: ListEvents' only predicate was on
// feed_id, so omitting it returned every tenant's feed events including
// their payloads.
func TestListEvents_NoFeedFilter_DoesNotLeakOtherTenants(t *testing.T) {
	r, _ := setupTestRouter(t)

	// Tenant A creates a subscription and ingests an event.
	subBody := []byte(`{"legal_entity_id":"le-a","provider":"BLOOMBERG","feed_type":"MARKET_DATA","symbol":"AAPL"}`)
	subReq := httptest.NewRequest("POST", "/v1/external-data-feeds/subscriptions", bytes.NewReader(subBody))
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Header.Set("X-Tenant-Id", "tenant-a")
	subReq.Header.Set("X-Principal-Id", "principal-01")
	subW := httptest.NewRecorder()
	r.ServeHTTP(subW, subReq)
	if subW.Code != http.StatusCreated {
		t.Fatalf("create subscription: expected 201, got %d: %s", subW.Code, subW.Body.String())
	}

	// Tenant B lists events with NO feed filter — the call that used to leak.
	req := httptest.NewRequest("GET", "/v1/external-data-feeds/events", nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("tenant-a")) {
		t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered event list mentioned tenant-a: %s", w.Body.String())
	}
}
