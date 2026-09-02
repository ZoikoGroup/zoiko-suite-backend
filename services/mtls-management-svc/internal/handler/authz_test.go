package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/mtls-management-svc/internal/authz"
)

// Authorization tests for mtls-management-svc (tracker row 88).
//
// Priority 1b gave this service a verified TENANT, which closed the
// cross-tenant hole. It left an intra-tenant one: any principal holding any
// valid envelope for the tenant could list every certificate in it and
// revoke or rotate any of them. On the service that issues the platform's
// mutual-TLS material, "any principal in the tenant" is the wrong audience
// for a revoke button — revocation breaks whatever service-to-service
// authentication depends on that certificate.
//
// What these tests pin that the behavioural suite cannot:
//
//  1. The authorization call is actually REACHED on every route. With a
//     granting stub every route behaves exactly as before, so only a
//     denying stub distinguishes "authorized" from "never asked".
//  2. Revocation is refused with the certificate still ACTIVE afterwards —
//     a 403 response with a revoked cert behind it would be worse than no
//     check at all.
//  3. The failure posture is fail-closed in both directions: denied is 403,
//     unreachable is 503.

func certRoutes(certID string) []struct {
	name, method, path string
	body               []byte
} {
	provision, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"service_name":    "probe-svc",
		"common_name":     "probe-svc.zoiko.internal",
		"rotation_days":   90,
		"auto_rotate":     false,
	})
	policy, _ := json.Marshal(map[string]interface{}{
		"policy_name":    "p1",
		"source_service": "a",
		"target_service": "b",
		"action":         "ALLOW",
		"requires_mtls":  true,
	})
	return []struct {
		name, method, path string
		body               []byte
	}{
		{"provision certificate", http.MethodPost, "/v1/mtls/certificates", provision},
		{"list certificates", http.MethodGet, "/v1/mtls/certificates?legal_entity_id=le-1", nil},
		{"get certificate", http.MethodGet, "/v1/mtls/certificates/" + certID, nil},
		{"rotate certificate", http.MethodPost, "/v1/mtls/certificates/" + certID + "/rotate", nil},
		{"revoke certificate", http.MethodDelete, "/v1/mtls/certificates/" + certID, nil},
		{"create policy", http.MethodPost, "/v1/mtls/policies", policy},
		{"list policies", http.MethodGet, "/v1/mtls/policies", nil},
	}
}

// TestMissingPrincipal_Refused covers the other half of the identity
// envelope. Priority 1b required a verified tenant here; nothing read
// X-Principal-Id on any route, so there was no notion of WHO was acting.
func TestMissingPrincipal_Refused(t *testing.T) {
	r := newRouter(t)
	certID := provisionCert(t, r, "t1", "seed-svc")

	for _, tc := range certRoutes(certID) {
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
				t.Fatalf("expected 401 with no X-Principal-Id, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthzDenied_RefusedOnEveryRoute proves the check is wired everywhere.
//
// One router throughout, with the decision flipped after seeding. That
// ordering matters: the certificate must genuinely EXIST before the denied
// calls, otherwise the by-id routes would 404 and a missing row would
// masquerade as a refusal.
func TestAuthzDenied_RefusedOnEveryRoute(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(t, az)
	certID := provisionCert(t, r, "t1", "seed-svc")

	az.err = authz.ErrAuthorizationDenied

	for _, tc := range certRoutes(certID) {
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
			req = withEnvelope(req) // rest of the canonical envelope; headers already set win
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 when authorization is DENIED, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestRevokeDenied_CertificateStaysActive is the assertion that matters most
// on this service. A 403 on the response is not the same as nothing having
// happened: revocation is destructive and irreversible from the caller's
// point of view, so the certificate must still be usable afterwards.
func TestRevokeDenied_CertificateStaysActive(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(t, az)
	certID := provisionCert(t, r, "t1", "critical-svc")

	az.err = authz.ErrAuthorizationDenied

	del := httptest.NewRequest(http.MethodDelete, "/v1/mtls/certificates/"+certID, nil)
	del.Header.Set("X-Tenant-ID", "t1")
	del.Header.Set("X-Principal-Id", "principal-denied")
	del = withEnvelope(del) // rest of the canonical envelope; headers already set win
	delW := httptest.NewRecorder()
	r.ServeHTTP(delW, del)
	if delW.Code != http.StatusForbidden {
		t.Fatalf("expected 403 revoking without authorization, got %d: %s", delW.Code, delW.Body.String())
	}

	// Re-grant only so the read can confirm the state; the point is what the
	// denied DELETE did or did not do.
	az.err = nil

	get := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates/"+certID, nil)
	get.Header.Set("X-Tenant-ID", "t1")
	get.Header.Set("X-Principal-Id", "principal-test-01")
	get = withEnvelope(get) // rest of the canonical envelope; headers already set win
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, get)
	if getW.Code != http.StatusOK {
		t.Fatalf("certificate should still be readable, got %d: %s", getW.Code, getW.Body.String())
	}
	var cert struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(getW.Body).Decode(&cert); err != nil {
		t.Fatalf("decode certificate: %v", err)
	}
	if cert.Status != "ACTIVE" {
		t.Fatalf("AUTHORIZATION FAILURE: a refused revoke still changed the certificate to %s", cert.Status)
	}
}

// TestAuthzUnavailable_FailsClosed pins the posture this change introduces:
// authorization-svc being unreachable turns a working call into a 503 rather
// than a silent success.
func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	r := newRouterWithAuthz(t, &stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"service_name":    "probe-svc",
		"common_name":     "probe-svc.zoiko.internal",
		"rotation_days":   90,
		"auto_rotate":     false,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListRequiresLegalEntity pins the API tightening. legal_entity_id is now
// the authorization scope for a listing, and an optional scope parameter is a
// scope that disables itself — the same shape as the self-disabling filters
// found across the six connector services in Priority 2. Omitting it used to
// return every certificate in the tenant across all legal entities.
func TestListRequiresLegalEntity(t *testing.T) {
	r := newRouter(t)
	provisionCert(t, r, "t1", "seed-svc")

	req := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 listing with no legal_entity_id, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("seed-svc")) {
		t.Fatalf("a rejected listing must not return certificates: %s", w.Body.String())
	}
}
