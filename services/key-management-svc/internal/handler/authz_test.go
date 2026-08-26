package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/key-management-svc/internal/authz"
)

// Authorization tests for key-management-svc (tracker row 90).
//
// Priority 1b gave this service a verified TENANT, closing the cross-tenant
// hole. It left an intra-tenant one: any principal holding any valid envelope
// for the tenant could list every customer key in it, and rotate or DISABLE
// any of them.
//
// Disable is the route that shapes the design. Tracker row 82 records that
// this service is metadata CRUD only — it never actually encrypts or decrypts
// anything — so today a disable just flips a status field. That is precisely
// why the boundary belongs here now: the moment these records drive real KMS
// operations, an unauthorized disable becomes unrecoverable data loss on
// everything the key protects, and nobody re-derives an authorization model
// while wiring up crypto.
//
// All 5 routes carry the guard, unlike siem-integration-svc's 3 of 5. That is
// not a judgement call — it was checked: nothing calls this service over HTTP
// (no KEY_MANAGEMENT_SERVICE_URL, no /v1/keys caller outside its own tree),
// so requiring a principal breaks no existing integration.

func seedKey(t *testing.T, r http.Handler, tenantID, alias string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"legal_entity_id":  "le-1",
		"key_alias":        alias,
		"key_model":        "BYOK",
		"key_provider":     "AWS_KMS",
		"external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/" + alias,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-seed")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed key: expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	return out.ID
}

func keyRoutes(keyID string) []struct {
	name, method, path string
	body               []byte
} {
	register, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "key_alias": "probe",
		"key_model": "BYOK", "key_provider": "AWS_KMS",
		"external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/probe",
	})
	return []struct {
		name, method, path string
		body               []byte
	}{
		{"register key", http.MethodPost, "/v1/keys", register},
		{"list keys", http.MethodGet, "/v1/keys?legal_entity_id=le-1", nil},
		{"get key", http.MethodGet, "/v1/keys/" + keyID, nil},
		{"rotate key", http.MethodPost, "/v1/keys/" + keyID + "/rotate", nil},
		{"disable key", http.MethodPost, "/v1/keys/" + keyID + "/disable", nil},
	}
}

func TestMissingPrincipal_Refused(t *testing.T) {
	r := newRouter()
	keyID := seedKey(t, r, "t1", "seed-key")

	for _, tc := range keyRoutes(keyID) {
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

// TestAuthzDenied_RefusedOnEveryRoute proves the check is reached on all five.
// The key is seeded with a granting decision and the stub flipped afterwards,
// so the row genuinely exists — otherwise a 404 could masquerade as a refusal.
func TestAuthzDenied_RefusedOnEveryRoute(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(az)
	keyID := seedKey(t, r, "t1", "seed-key")

	az.err = authz.ErrAuthorizationDenied

	for _, tc := range keyRoutes(keyID) {
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

// TestDisableDenied_KeyStaysEnabled is the assertion that matters most here.
// A 403 in the response is not the same as nothing having happened: disable is
// the destructive route, so the key must still be usable afterwards.
//
// This is also what makes the read-before-mutate ordering testable.
// store.DisableKey returns only an error, so before this change there was no
// way to know whose key was being disabled until after disabling it — the
// authorization decision had to be moved ahead of the mutation.
func TestDisableDenied_KeyStaysEnabled(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(az)
	keyID := seedKey(t, r, "t1", "critical-key")

	az.err = authz.ErrAuthorizationDenied

	req := httptest.NewRequest(http.MethodPost, "/v1/keys/"+keyID+"/disable", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-denied")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 disabling without authorization, got %d: %s", w.Code, w.Body.String())
	}

	// Re-grant only so the state can be read back; the point is what the
	// refused disable did or did not do.
	az.err = nil

	get := httptest.NewRequest(http.MethodGet, "/v1/keys/"+keyID, nil)
	get.Header.Set("X-Tenant-ID", "t1")
	get.Header.Set("X-Principal-Id", "principal-test-01")
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, get)
	if getW.Code != http.StatusOK {
		t.Fatalf("key should still be readable, got %d: %s", getW.Code, getW.Body.String())
	}
	var key struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(getW.Body).Decode(&key); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if key.State == "DISABLED" {
		t.Fatalf("AUTHORIZATION FAILURE: a refused disable still disabled the key")
	}
}

// TestRotateDenied_VersionUnchanged is the same argument for rotation: the
// version counter must not advance on a refused request.
func TestRotateDenied_VersionUnchanged(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(az)
	keyID := seedKey(t, r, "t1", "rotating-key")

	az.err = authz.ErrAuthorizationDenied

	req := httptest.NewRequest(http.MethodPost, "/v1/keys/"+keyID+"/rotate", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-denied")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 rotating without authorization, got %d: %s", w.Code, w.Body.String())
	}

	az.err = nil
	get := httptest.NewRequest(http.MethodGet, "/v1/keys/"+keyID, nil)
	get.Header.Set("X-Tenant-ID", "t1")
	get.Header.Set("X-Principal-Id", "principal-test-01")
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, get)
	var key struct {
		KeyVersion    int `json:"key_version"`
		RotationCount int `json:"rotation_count"`
	}
	if err := json.NewDecoder(getW.Body).Decode(&key); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if key.KeyVersion != 1 || key.RotationCount != 0 {
		t.Fatalf("AUTHORIZATION FAILURE: a refused rotate still advanced the key (version=%d rotations=%d)",
			key.KeyVersion, key.RotationCount)
	}
}

// TestAuthzUnavailable_FailsClosed pins the posture: unreachable
// authorization-svc is a 503, not a silent success.
func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	r := newRouterWithAuthz(&stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	body, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "key_alias": "k",
		"key_model": "BYOK", "key_provider": "AWS_KMS",
		"external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/k",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListRequiresLegalEntity pins the API tightening: legal_entity_id is the
// authorization scope, and an optional scope parameter is a scope that
// disables itself.
func TestListRequiresLegalEntity(t *testing.T) {
	r := newRouter()
	seedKey(t, r, "t1", "seed-key")

	req := httptest.NewRequest(http.MethodGet, "/v1/keys", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 listing with no legal_entity_id, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("seed-key")) {
		t.Fatalf("a rejected listing must not return keys: %s", w.Body.String())
	}
}
