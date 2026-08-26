package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/siem-integration-svc/internal/authz"
)

// Authorization and credential-redaction tests for siem-integration-svc
// (tracker rows 89 and 8p-a).
//
// Two separate defects were fixed here and they need separate tests, because
// each would pass while the other was broken:
//
//  1. auth_token — a live credential for the tenant's SIEM platform — was
//     returned by every exporter read. domain.SIEMExporter.AuthToken is now
//     json:"-" so the wire format cannot carry it at all.
//  2. No route had any authorization. Three of the five now do.
//
// The asymmetry in (2) is deliberate and tested for below: POST /stream and
// GET /exporters are called by five other services whose clients send only
// X-Tenant-ID, so requiring a principal there would silently break
// security-event streaming platform-wide. TestServiceToServiceRoutesStillWork
// pins that, so a future "consistency" change fails loudly instead of taking
// out the detection pipeline.

func seedExporter(t *testing.T, r http.Handler, tenantID, token string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1",
		"name":            "primary",
		"platform":        "SPLUNK",
		"endpoint_url":    "https://splunk.example/services/collector",
		"auth_token":      token,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-seed")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed exporter: expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode exporter: %v", err)
	}
	return out.ID
}

// TestAuthTokenNeverSerialised is row 8p-a. The token must not appear in ANY
// response — including the create response that supplied it.
func TestAuthTokenNeverSerialised(t *testing.T) {
	r := newRouter()
	const secret = "splunk-hec-token-DO-NOT-LEAK"

	// Create — the response echoes the exporter back.
	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1",
		"name":            "primary",
		"platform":        "SPLUNK",
		"endpoint_url":    "https://splunk.example/services/collector",
		"auth_token":      secret,
	})
	createReq := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-Tenant-ID", "t1")
	createReq.Header.Set("X-Principal-Id", "principal-test-01")
	createW := httptest.NewRecorder()
	r.ServeHTTP(createW, createReq)
	if createW.Code != http.StatusCreated {
		t.Fatalf("create exporter: expected 201, got %d — %s", createW.Code, createW.Body.String())
	}
	if bytes.Contains(createW.Body.Bytes(), []byte(secret)) {
		t.Fatalf("CREDENTIAL LEAK: create response echoed the auth token: %s", createW.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createW.Body).Decode(&created); err != nil {
		t.Fatalf("decode created exporter: %v", err)
	}

	// Get and List — both previously returned the token.
	for _, path := range []string{
		"/v1/siem/exporters/" + created.ID,
		"/v1/siem/exporters?legal_entity_id=le-1",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-ID", "t1")
		req.Header.Set("X-Principal-Id", "principal-test-01")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d — %s", path, w.Code, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte(secret)) {
			t.Fatalf("CREDENTIAL LEAK: %s returned the auth token: %s", path, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte("auth_token")) {
			t.Fatalf("%s: the auth_token field must not appear at all: %s", path, w.Body.String())
		}
		// The endpoint_url SHOULD still be there — this test must fail if the
		// response were simply empty, which would pass the assertions above
		// for the wrong reason.
		if !bytes.Contains(w.Body.Bytes(), []byte("splunk.example")) {
			t.Fatalf("%s: expected the exporter body to still contain endpoint_url: %s", path, w.Body.String())
		}
	}
}

// TestMissingPrincipal_RefusedOnGuardedRoutes covers the three routes that
// now require a principal. The other two are covered by
// TestServiceToServiceRoutesStillWork below.
func TestMissingPrincipal_RefusedOnGuardedRoutes(t *testing.T) {
	r := newRouter()
	expID := seedExporter(t, r, "t1", "tok")

	createBody, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "name": "x", "platform": "SPLUNK",
		"endpoint_url": "https://x.example",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create exporter", http.MethodPost, "/v1/siem/exporters", createBody},
		{"get exporter", http.MethodGet, "/v1/siem/exporters/" + expID, nil},
		{"list events", http.MethodGet, "/v1/siem/events?exporter_id=" + expID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Tenant-ID", "t1")
			// Deliberately NO X-Principal-Id.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no principal, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestServiceToServiceRoutesStillWork is the guard rail on the asymmetry.
//
// POST /stream and GET /exporters must keep working with a tenant header
// ALONE, because that is exactly what authorization-svc, gateway-auth-svc,
// identity-context-svc, key-management-svc and mtls-management-svc send. If
// someone later adds requirePrincipal to either route "for consistency",
// this test fails instead of the platform quietly losing its security-event
// pipeline.
func TestServiceToServiceRoutesStillWork(t *testing.T) {
	r := newRouter()
	expID := seedExporter(t, r, "t1", "tok")

	// GET /exporters — how a service discovers where to stream.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters", nil)
	listReq.Header.Set("X-Tenant-ID", "t1")
	// Deliberately NO principal — this is a service-to-service call.
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("GET /exporters must work with a tenant header alone (5 services depend on it), got %d: %s",
			listW.Code, listW.Body.String())
	}

	// POST /stream — how a service emits a security event.
	streamBody, _ := json.Marshal(map[string]string{
		"exporter_id": expID, "source_service": "gateway-auth-svc",
		"event_type": "authentication.failed", "severity": "HIGH",
		"message": "invalid credentials",
	})
	streamReq := httptest.NewRequest(http.MethodPost, "/v1/siem/stream", bytes.NewReader(streamBody))
	streamReq.Header.Set("Content-Type", "application/json")
	streamReq.Header.Set("X-Tenant-ID", "t1")
	// Deliberately NO principal. gateway-auth-svc reporting an AUTHENTICATION
	// FAILURE has no authenticated principal to send, by definition.
	streamW := httptest.NewRecorder()
	r.ServeHTTP(streamW, streamReq)
	if streamW.Code != http.StatusCreated {
		t.Fatalf("POST /stream must work with a tenant header alone, got %d: %s",
			streamW.Code, streamW.Body.String())
	}
}

// TestAuthzDenied_RefusedOnGuardedRoutes proves the check is reached. With a
// granting stub every route behaves as before, so only a denial distinguishes
// "authorized" from "never asked".
func TestAuthzDenied_RefusedOnGuardedRoutes(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(az)
	expID := seedExporter(t, r, "t1", "tok")

	az.err = authz.ErrAuthorizationDenied

	createBody, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "name": "x", "platform": "SPLUNK",
		"endpoint_url": "https://x.example",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create exporter", http.MethodPost, "/v1/siem/exporters", createBody},
		{"get exporter", http.MethodGet, "/v1/siem/exporters/" + expID, nil},
		{"list events", http.MethodGet, "/v1/siem/events?exporter_id=" + expID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Tenant-ID", "t1")
			req.Header.Set("X-Principal-Id", "principal-denied")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 when authorization is DENIED, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthzUnavailable_FailsClosed pins the posture: unreachable
// authorization-svc is a 503, not a silent success.
func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	r := newRouterWithAuthz(&stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "name": "x", "platform": "SPLUNK",
		"endpoint_url": "https://x.example",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListEventsRequiresExporterID pins the API tightening: exporter_id is
// the authorization scope, and an optional scope parameter is a scope that
// disables itself.
func TestListEventsRequiresExporterID(t *testing.T) {
	r := newRouter()
	seedExporter(t, r, "t1", "tok")

	req := httptest.NewRequest(http.MethodGet, "/v1/siem/events", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 listing events with no exporter_id, got %d: %s", w.Code, w.Body.String())
	}
}
