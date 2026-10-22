package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	authzpkg "zoiko.io/privacy-transfer-svc/internal/authz"
	"zoiko.io/privacy-transfer-svc/internal/domain"
)

// Documented pre-flight order (README): the tenant middleware wraps every API
// route and therefore runs BEFORE the handler, then the handler checks the
// principal, then (mutating) correlation id and idempotency key, then the
// authorization grant:
//
//	1. X-Tenant-Id (middleware)  -> 401
//	2. X-Principal-Id            -> 401
//	3. X-Correlation-ID          -> 400   (mutating)
//	4. Idempotency-Key           -> 400   (mutating)
//	5. authorization grant       -> 403 / 503
//
// A request with neither the tenant nor the principal is therefore refused for
// the TENANT. ZS-SVC-W-001 §18.1 requires both headers but prescribes no order
// between them; these tests pin the order the service actually applies.

type orderAuthz struct {
	mu    sync.Mutex
	deny  bool
	calls int
}

func (a *orderAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func errMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not a JSON error body: %q", w.Body.String())
	}
	return m["error"]
}

func TestPreflightOrder_TenantMiddlewareFirst_ThenPrincipal(t *testing.T) {
	az := &orderAuthz{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, newStubPurposeRegistry())
	body := domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}
	const tenantMsg, principalMsg = "X-Tenant-Id header is required", "X-Principal-Id header is required"
	mut := map[string]string{"Idempotency-Key": "k", "X-Correlation-ID": "c"}
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{}
		for k, v := range mut {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"neither principal nor tenant: refused for the tenant", with(map[string]string{"X-Principal-Id": ""}), tenantMsg},
		{"principal but no tenant", with(nil), tenantMsg},
		{"whitespace tenant", with(map[string]string{"X-Tenant-Id": "   "}), tenantMsg},
		{"tenant but no principal", with(map[string]string{"X-Tenant-Id": testTenant, "X-Principal-Id": ""}), principalMsg},
		{"tenant and whitespace principal", with(map[string]string{"X-Tenant-Id": testTenant, "X-Principal-Id": "  "}), principalMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRawRequest(r, http.MethodPost, "/privacy/processor-relationships", body, tc.headers)
			if w.Code != http.StatusUnauthorized || errMessage(t, w) != tc.want {
				t.Fatalf("want 401 %q, got %d %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
	if az.calls != 0 {
		t.Fatalf("authorization must not be consulted before both headers are present (%d calls)", az.calls)
	}

	// Both present: the request proceeds to the remaining checks and is processed.
	w := doRawRequest(r, http.MethodPost, "/privacy/processor-relationships", body, with(map[string]string{"X-Tenant-Id": testTenant}))
	if w.Code != http.StatusCreated {
		t.Fatalf("principal and tenant present: want 201, got %d %s", w.Code, w.Body.String())
	}
	if az.calls != 1 {
		t.Fatalf("authorization must be consulted exactly once, saw %d", az.calls)
	}
}

// After the two 401 checks the order continues: correlation id, then idempotency
// key, then authorization — and authorization still precedes any lookup.
func TestPreflightOrder_RemainingStepsUnchanged(t *testing.T) {
	az := &orderAuthz{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, newStubPurposeRegistry())
	body := domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}
	base := map[string]string{"X-Tenant-Id": testTenant}

	w := doRawRequest(r, http.MethodPost, "/privacy/processor-relationships", body, base)
	if w.Code != http.StatusBadRequest || errMessage(t, w) != "X-Correlation-ID header is required" {
		t.Fatalf("correlation id is checked first: %d %s", w.Code, w.Body.String())
	}
	w = doRawRequest(r, http.MethodPost, "/privacy/processor-relationships", body, map[string]string{"X-Tenant-Id": testTenant, "X-Correlation-ID": "c"})
	if w.Code != http.StatusBadRequest || errMessage(t, w) != "Idempotency-Key header is required" {
		t.Fatalf("idempotency key is checked next: %d %s", w.Code, w.Body.String())
	}
	if az.calls != 0 {
		t.Fatalf("authorization comes after the header checks (%d calls)", az.calls)
	}

	// A denied caller gets 403 for an id that exists and for one that does not.
	rel := createRelationship(t, r)
	az.deny = true
	for _, id := range []string{rel.RelationshipID, "00000000-0000-0000-0000-000000000099"} {
		w := doRawRequest(r, http.MethodGet, "/privacy/processor-relationships/"+id, nil, base)
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET %s while denied: want 403, got %d", id, w.Code)
		}
	}
}
