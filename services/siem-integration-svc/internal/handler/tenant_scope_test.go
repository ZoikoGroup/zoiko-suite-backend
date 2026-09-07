package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tenant-scope tests for siem-integration-svc (tracker row 8p).
//
// The store is correctly scoped on every read and mutation. The single
// defect was a getTenant helper returning the literal "default-tenant" when
// X-Tenant-ID was absent — so the correct store faithfully enforced a
// synthetic tenant shared by every header-less caller.
//
// This service held the worst payload of the three security services in
// this tier, for two reasons:
//
//   - A SIEMExporter carries endpoint_url AND auth_token. The token is
//     stored as supplied and never redacted on read, so the shared bucket
//     exposed a live credential for another tenant's SIEM destination —
//     not metadata about a secret, the secret itself.
//   - ListEvents is the security event stream. A tenant's detection
//     pipeline was readable by anyone who omitted a header, which is
//     precisely the pipeline meant to catch that.
//
// No RLS dimension: no migrations and no Postgres here.

func createExporter(t *testing.T, r http.Handler, tenantID, name, token string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1",
		"name":            name,
		"platform":        "SPLUNK",
		"endpoint_url":    "https://" + name + ".splunk.example/services/collector",
		"auth_token":      token,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create exporter for %s: expected 201, got %d — %s", tenantID, w.Code, w.Body.String())
	}
	var exp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&exp); err != nil {
		t.Fatalf("decode exporter: %v", err)
	}
	return exp.ID
}

func TestMissingTenantHeader_Refused(t *testing.T) {
	r := newRouter()

	exporterBody, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "name": "e1", "platform": "SPLUNK",
		"endpoint_url": "https://e1.splunk.example/services/collector",
		"auth_token":   "tok-1",
	})
	streamBody, _ := json.Marshal(map[string]string{
		"exporter_id": "some-id", "source_service": "svc-a",
		"event_type": "test.event", "severity": "LOW", "message": "hello",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create exporter", http.MethodPost, "/v1/siem/exporters", exporterBody},
		{"list exporters", http.MethodGet, "/v1/siem/exporters", nil},
		{"get exporter", http.MethodGet, "/v1/siem/exporters/some-id", nil},
		{"stream event", http.MethodPost, "/v1/siem/stream", streamBody},
		{"list events", http.MethodGet, "/v1/siem/events", nil},
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

// TestForeignTenant_CannotReadExporterAuthToken is the most important
// assertion in this file: the exporter's auth_token is a live credential and
// is not redacted on read, so it must never appear in another tenant's
// response.
func TestForeignTenant_CannotReadExporterAuthToken(t *testing.T) {
	r := newRouter()

	const secret = "tenant-a-splunk-hec-token"
	expID := createExporter(t, r, "tenant-a", "tenant-a-exporter", secret)

	// Direct read by id.
	req := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters/"+expID, nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's exporter, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(secret)) {
		t.Fatalf("ISOLATION FAILURE: tenant-b read tenant-a's SIEM auth_token: %s", w.Body.String())
	}

	// Unfiltered list.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-b")
	listReq.Header.Set("X-Principal-Id", "principal-test-01")
	listReq = withEnvelope(listReq) // rest of the canonical envelope; headers already set win
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listW.Code, listW.Body.String())
	}
	for _, needle := range []string{secret, "tenant-a-exporter"} {
		if bytes.Contains(listW.Body.Bytes(), []byte(needle)) {
			t.Fatalf("ISOLATION FAILURE: tenant-b's exporter list contained %q: %s", needle, listW.Body.String())
		}
	}

	// Sanity: tenant-a still gets its own.
	ownReq := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters/"+expID, nil)
	ownReq.Header.Set("X-Tenant-ID", "tenant-a")
	ownReq.Header.Set("X-Principal-Id", "principal-test-01")
	ownReq = withEnvelope(ownReq) // rest of the canonical envelope; headers already set win
	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, ownReq)
	if ownW.Code != http.StatusOK {
		t.Fatalf("tenant-a must still read its own exporter, got %d: %s", ownW.Code, ownW.Body.String())
	}
}

// TestForeignTenant_CannotReadOthersSecurityEvents covers the event stream —
// a tenant's detection pipeline must not be readable by another tenant.
func TestForeignTenant_CannotReadOthersSecurityEvents(t *testing.T) {
	r := newRouter()

	expID := createExporter(t, r, "tenant-a", "tenant-a-exporter", "tok-a")

	streamBody, _ := json.Marshal(map[string]string{
		"exporter_id": expID, "source_service": "authorization-svc",
		"event_type": "authorization.denied", "severity": "HIGH",
		"message": "tenant-a-sensitive-detection-detail",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/stream", bytes.NewReader(streamBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("stream event as tenant-a: expected 201, got %d — %s", w.Code, w.Body.String())
	}

	// Tenant B asks for the event list using TENANT-A's exporter id.
	//
	// This is a stronger assertion than the unfiltered list it replaces.
	// exporter_id is now required (it is the authorization scope), so the
	// interesting question is no longer "does an unscoped list leak" but
	// "does naming the victim's exporter directly get you anywhere". It must
	// not: the store is tenant-scoped, so tenant-b's lookup of tenant-a's
	// exporter finds nothing and the request stops at 404 — before the
	// authorization check, and long before any event is read.
	//
	// 404 rather than 403 is deliberate. A distinct forbidden would confirm
	// to a prober that the exporter id exists.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/siem/events?exporter_id="+expID, nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-b")
	listReq.Header.Set("X-Principal-Id", "principal-test-01")
	listReq = withEnvelope(listReq) // rest of the canonical envelope; headers already set win
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's exporter, got %d: %s", listW.Code, listW.Body.String())
	}
	if bytes.Contains(listW.Body.Bytes(), []byte("tenant-a-sensitive-detection-detail")) {
		t.Fatalf("ISOLATION FAILURE: tenant-b read tenant-a's security events: %s", listW.Body.String())
	}

	// Sanity: tenant-a can read its own events through the same route, so
	// the refusal above is isolation rather than a route that never works.
	ownList := httptest.NewRequest(http.MethodGet, "/v1/siem/events?exporter_id="+expID, nil)
	ownList.Header.Set("X-Tenant-ID", "tenant-a")
	ownList.Header.Set("X-Principal-Id", "principal-test-01")
	ownList = withEnvelope(ownList) // rest of the canonical envelope; headers already set win
	ownListW := httptest.NewRecorder()
	r.ServeHTTP(ownListW, ownList)
	if ownListW.Code != http.StatusOK {
		t.Fatalf("tenant-a must read its own events, got %d: %s", ownListW.Code, ownListW.Body.String())
	}
	if !bytes.Contains(ownListW.Body.Bytes(), []byte("tenant-a-sensitive-detection-detail")) {
		t.Fatalf("tenant-a's own event list should contain its event: %s", ownListW.Body.String())
	}

	var resp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(listW.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 0 {
		t.Fatalf("expected tenant-b to see no events, got %d", resp.Count)
	}
}

// TestForeignTenant_CannotStreamIntoOthersExporter covers the write side: a
// caller must not be able to inject events into another tenant's SIEM
// destination, which would let them forge or flood that tenant's detection
// feed.
func TestForeignTenant_CannotStreamIntoOthersExporter(t *testing.T) {
	r := newRouter()

	expID := createExporter(t, r, "tenant-a", "tenant-a-exporter", "tok-a")

	streamBody, _ := json.Marshal(map[string]string{
		"exporter_id": expID, "source_service": "svc-b",
		"event_type": "forged.event", "severity": "LOW", "message": "forged-by-tenant-b",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/stream", bytes.NewReader(streamBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Assert 404 specifically, not merely "not 201". An earlier draft of this
	// test checked only that the request was not created, and it passed while
	// actually failing on a 400 field-validation error — a false pass that
	// would have survived the tenant check being removed entirely. 404 is the
	// tenant-scoped store reporting that this exporter does not exist for
	// this caller.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 streaming into another tenant's exporter, got %d: %s", w.Code, w.Body.String())
	}

	// And tenant-a's feed must be clean.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/siem/events", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-a")
	listReq.Header.Set("X-Principal-Id", "principal-test-01")
	listReq = withEnvelope(listReq) // rest of the canonical envelope; headers already set win
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if bytes.Contains(listW.Body.Bytes(), []byte("forged-by-tenant-b")) {
		t.Fatalf("ISOLATION FAILURE: tenant-b's event landed in tenant-a's feed: %s", listW.Body.String())
	}
}

// TestTwoHeaderlessCallersDoNotShareANamespace is the direct regression test
// for the old behaviour.
func TestTwoHeaderlessCallersDoNotShareANamespace(t *testing.T) {
	r := newRouter()

	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "name": "shared-bucket-exporter", "platform": "SPLUNK",
		"endpoint_url": "https://shared.splunk.example/services/collector",
		"auth_token":   "shared-bucket-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("header-less create must be refused, got %d: %s", w.Code, w.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusUnauthorized {
		t.Fatalf("header-less list must be refused, got %d: %s", listW.Code, listW.Body.String())
	}
	if bytes.Contains(listW.Body.Bytes(), []byte("shared-bucket-token")) {
		t.Fatalf("ISOLATION FAILURE: a header-less caller saw another header-less caller's auth_token: %s", listW.Body.String())
	}
}
