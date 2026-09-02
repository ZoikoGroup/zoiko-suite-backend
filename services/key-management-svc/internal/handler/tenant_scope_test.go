package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
)

// scopeStubAuthz is a test double for AuthzChecker in the internal test
// package. GRANTS by default — these tests exercise TENANT isolation, and an
// authorization denial here would mask what they are actually asserting.
type scopeStubAuthz struct{}

func (s *scopeStubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return nil }

// Tenant-scope tests for key-management-svc (tracker row 8k).
//
// This service had exactly one isolation defect, and it was not in the
// store: every store method is correctly scoped (GetKeyByID, ListKeys,
// RotateKey and DisableKey all compare k.TenantID). The defect was a
// getTenant helper that returned the literal "default-tenant" when
// X-Tenant-ID was absent.
//
// That combination is the interesting part. Because the store is correct,
// it faithfully enforced the fabricated tenant as a real one — so every
// header-less caller shared a single key namespace and could act on each
// other's keys through it. A correctly written store plus one fabricated
// identity produced a shared control plane over key material.
//
// There is no RLS dimension here: this service has no migrations and no
// Postgres (see tracker row 82 — it is metadata CRUD only, and never
// actually encrypts or decrypts anything).

func newTestRouter() http.Handler {
	return NewRouter(New(store.NewMemoryStore(), siem.New("", "key-management-svc", zap.NewNop()), &scopeStubAuthz{}, zap.NewNop()))
}

func registerKey(t *testing.T, r http.Handler, tenantID, alias string) string {
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
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register key for %s: expected 201, got %d — %s", tenantID, w.Code, w.Body.String())
	}
	var key struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&key); err != nil {
		t.Fatalf("decode registered key: %v", err)
	}
	return key.ID
}

// TestMissingTenantHeader_Refused covers the fabricated-identity fix on
// every route, including the two that mutate key state.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r := newTestRouter()

	regBody, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "key_alias": "k1",
		"key_model": "BYOK", "key_provider": "AWS_KMS",
		"external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/k1",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"register key", http.MethodPost, "/v1/keys", regBody},
		{"list keys", http.MethodGet, "/v1/keys", nil},
		{"get key", http.MethodGet, "/v1/keys/some-id", nil},
		{"rotate key", http.MethodPost, "/v1/keys/some-id/rotate", nil},
		{"disable key", http.MethodPost, "/v1/keys/some-id/disable", nil},
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

// TestForeignTenant_CannotDisableKey is the one that matters most. Disabling
// a key is a denial of service on everything that key protects, so this
// asserts both that the request is refused AND that the key really is
// untouched afterwards — a 404 that still mutated state would pass a
// status-code-only test.
func TestForeignTenant_CannotDisableKey(t *testing.T) {
	r := newTestRouter()

	keyID := registerKey(t, r, "tenant-a", "tenant-a-key")

	for _, route := range []string{"/v1/keys/" + keyID + "/disable", "/v1/keys/" + keyID + "/rotate"} {
		req := httptest.NewRequest(http.MethodPost, route, nil)
		req.Header.Set("X-Tenant-ID", "tenant-b")
		req.Header.Set("X-Principal-Id", "principal-test-01")
		req = withEnvelope(req) // rest of the canonical envelope; headers already set win
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s as tenant-b: expected 404, got %d — %s", route, w.Code, w.Body.String())
		}
	}

	// tenant-a's key must still be there and still usable.
	req := httptest.NewRequest(http.MethodGet, "/v1/keys/"+keyID, nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant-a must still read its own key, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		KeyAlias string `json:"key_alias"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyAlias != "tenant-a-key" {
		t.Fatalf("unexpected key returned to tenant-a: %+v", got)
	}
	if got.Status == "DISABLED" {
		t.Fatalf("ISOLATION FAILURE: tenant-b's refused request still disabled tenant-a's key")
	}
}

// TestForeignTenant_CannotListOthersKeys covers the read side. The unfiltered
// list is the call that would have leaked under the fabricated tenant.
func TestForeignTenant_CannotListOthersKeys(t *testing.T) {
	r := newTestRouter()

	registerKey(t, r, "tenant-a", "tenant-a-key")
	registerKey(t, r, "tenant-b", "tenant-b-key")

	// legal_entity_id is now mandatory — it is the authorization scope. Both
	// tenants registered under le-1, so tenant-b asking for le-1 is the
	// sharper test: same legal entity, different tenant.
	req := httptest.NewRequest(http.MethodGet, "/v1/keys?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req = withEnvelope(req) // rest of the canonical envelope; headers already set win
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-key")) {
		t.Fatalf("ISOLATION FAILURE: tenant-b's key list contained tenant-a's key: %s", w.Body.String())
	}

	var resp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected tenant-b to see exactly its own 1 key, got %d", resp.Count)
	}
}

// TestTwoHeaderlessCallersDoNotShareANamespace is the regression test for the
// specific shape of the old bug: two callers with no tenant header must not
// end up in the same bucket. Before the fix both became "default-tenant" and
// the second caller could see and disable the first caller's key.
func TestTwoHeaderlessCallersDoNotShareANamespace(t *testing.T) {
	r := newTestRouter()

	regBody, _ := json.Marshal(map[string]string{
		"legal_entity_id": "le-1", "key_alias": "shared-bucket-key",
		"key_model": "BYOK", "key_provider": "AWS_KMS",
		"external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/shared",
	})

	// Caller 1, no header: must not even be able to create.
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewReader(regBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("header-less create must be refused, got %d: %s", w.Code, w.Body.String())
	}

	// Caller 2, no header: must not be able to list anything either — under
	// the old behaviour this returned caller 1's key.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/keys", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusUnauthorized {
		t.Fatalf("header-less list must be refused, got %d: %s", listW.Code, listW.Body.String())
	}
	if bytes.Contains(listW.Body.Bytes(), []byte("shared-bucket-key")) {
		t.Fatalf("ISOLATION FAILURE: a header-less caller saw another header-less caller's key: %s", listW.Body.String())
	}
}

// withEnvelope fills the canonical input contract headers this file's setup
// requests do not set. It duplicates the helper in envelope_contract_test.go
// deliberately: that file is in package handler_test and this one is in
// package handler, so the identifier is not shared.
//
// Only absent fields are filled, so the negative cases here - foreign tenant,
// missing principal - still say exactly what they mean.
func withEnvelope(r *http.Request) *http.Request {
	set := func(k, v string) {
		if r.Header.Get(k) == "" {
			r.Header.Set(k, v)
		}
	}
	set("X-Tenant-Id", "tenant-test")
	set("X-Principal-Id", "principal-test")
	set("X-Legal-Entity-Id", "entity-test")
	set("X-Request-Id", "req-test")
	set("X-Correlation-ID", "corr-test")
	set("X-Source-Channel", "api")
	set("X-Purpose-Context", "AUTOMATED_TEST")

	// A fresh key per request: reusing one would make the second call a replay,
	// which is what INV-08 asks the service to collapse.
	if r.Header.Get("Idempotency-Key") == "" {
		scopeIdempotencySeq++
		r.Header.Set("Idempotency-Key", fmt.Sprintf("idem-scope-%d", scopeIdempotencySeq))
	}
	return r
}

var scopeIdempotencySeq int
