package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/events"
	"zoiko.io/tax-authority-interface-svc/internal/handler"
	"zoiko.io/tax-authority-interface-svc/internal/idempotency"
	"zoiko.io/tax-authority-interface-svc/internal/router"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

// memIdem is an in-memory idempotency.Store with PgStore's semantics.
type memIdem struct {
	mu sync.Mutex
	m  map[string]*idempotency.Record
}

func newMemIdem() *memIdem { return &memIdem{m: map[string]*idempotency.Record{}} }
func (s *memIdem) Claim(_ context.Context, t, e, k, fp string) (*idempotency.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := t + "|" + e + "|" + k
	if r, ok := s.m[id]; ok {
		if r.RequestFingerprint != fp {
			return nil, idempotency.ErrFingerprintMismatch
		}
		c := *r
		return &c, nil
	}
	s.m[id] = &idempotency.Record{RequestFingerprint: fp}
	return nil, nil
}
func (s *memIdem) Complete(_ context.Context, t, e, k string, st int, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[t+"|"+e+"|"+k]
	r.ResponseStatus, r.ResponseBody = st, append([]byte(nil), b...)
	return nil
}
func (s *memIdem) Release(_ context.Context, t, e, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[t+"|"+e+"|"+k]; ok && r.ResponseStatus == 0 {
		delete(s.m, t+"|"+e+"|"+k)
	}
	return nil
}

// policyAuthz denies exactly the actions in deny and grants everything else.
func policyAuthz(t *testing.T, deny ...string) *authz.Client {
	denied := map[string]bool{}
	for _, a := range deny {
		denied[a] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ActionType string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := "GRANTED"
		if denied[req.ActionType] {
			out = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}))
	t.Cleanup(srv.Close)
	return authz.NewClient(srv.URL)
}

// newRouter wires the router as cmd/server does with the default config:
// the live authority adapter is NOT enabled.
func newRouter(t *testing.T, db pinger, az *authz.Client) http.Handler {
	h := handler.New(store.NewMemoryStore(), events.NewMockPublisher(), az, zap.NewNop())
	return router.New(router.Deps{DB: db, Idem: newMemIdem(), Handler: h, Logger: zap.NewNop()})
}

var keySeq int

func call(h http.Handler, method, path, tenant string, body any) *httptest.ResponseRecorder {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "principal-01")
	req.Header.Set("X-Request-Id", "req-1")
	req.Header.Set("X-Correlation-ID", "corr-1")
	req.Header.Set("X-Source-Channel", "api")
	req.Header.Set("X-Legal-Entity-Id", "le-1")
	req.Header.Set("X-Purpose-Context", "tax-filing")
	keySeq++
	req.Header.Set("Idempotency-Key", fmt.Sprintf("key-%d", keySeq))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// routerMiss: chi's own 404 is text/plain "404 page not found"; handler 404s are JSON.
func routerMiss(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusMethodNotAllowed ||
		(w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "404 page not found"))
}

func createIface(t *testing.T, h http.Handler, tenant string) domain.TaxInterface {
	t.Helper()
	w := call(h, "POST", "/v1/tax-authority/interfaces", tenant, domain.CreateInterfaceRequest{
		LegalEntityID: "le-1", Jurisdiction: "GB", AuthorityName: "HMRC MTD", Protocol: "REST/OAuth2"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create interface: %d %s", w.Code, w.Body.String())
	}
	var tf domain.TaxInterface
	_ = json.Unmarshal(w.Body.Bytes(), &tf)
	return tf
}

func TestProbes_PublicAndReadyzReflectsDB(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s without headers: want 200, got %d: %s", p, w.Code, w.Body.String())
		}
	}
	down := newRouter(t, pinger{err: errors.New("db down")}, policyAuthz(t))
	w := httptest.NewRecorder()
	down.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"unavailable"`) {
		t.Fatalf("readyz with DB down: want 503 unavailable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRouteContract(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	tf := createIface(t, h, "tenant-a")
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/tax-authority/interfaces", nil},
		{"GET", "/v1/tax-authority/interfaces/" + tf.InterfaceID, nil},
		{"POST", "/v1/tax-authority/filings", domain.SubmitTaxFilingRequest{InterfaceID: tf.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT_RETURN", TaxAmount: 10}},
		{"GET", "/v1/tax-authority/filings", nil},
		{"GET", "/v1/tax-authority/filings/f-1", nil},
		{"POST", "/v1/tax-authority/filings/f-1/validate", nil},
		{"POST", "/v1/tax-authority/filings/f-1/submit", nil},
		{"POST", "/v1/tax-authority/filings/f-1/poll", nil},
		{"GET", "/v1/tax-authority/filings/f-1/evidence", nil},
		{"GET", "/v1/tax-authority/filings/f-1/receipt", nil},
		{"GET", "/v1/tax-authority/callbacks", nil},
		{"GET", "/v1/tax-authority/health", nil},
		{"POST", "/v1/tax-authority/provider-profiles/hmrc-mtd-vat", map[string]string{}},
		{"GET", "/v1/tax-authority/provider-profiles/", nil},
		{"GET", "/v1/tax-authority/provider-profiles/hmrc-mtd-vat", nil},
		{"POST", "/v1/tax-authority/provider-profiles/hmrc-mtd-vat/1/certify", nil},
		{"POST", "/v1/tax-authority/provider-profiles/hmrc-mtd-vat/1/deprecate", nil},
		{"POST", "/v1/tax-authority/provider-profiles/hmrc-mtd-vat/1/withdraw", nil},
		{"GET", "/v1/control-populations/filings?legal_entity_id=le-1", nil},
		{"POST", "/v1/tax-authority/callbacks/hmrc/tenant-a", map[string]string{}},
	}
	for _, rt := range routes {
		w := call(h, rt.method, rt.path, "tenant-a", rt.body)
		if routerMiss(w) {
			t.Errorf("%s %s: router answered %d (%s)", rt.method, rt.path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

// With the default config the live adapter is off; its routes say so with
// 503 instead of pretending to work or hiding behind 404.
func TestAuthorityAdapterNotConfigured_503(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	for _, p := range []string{"/v1/tax-authority/filings/f-1/submit", "/v1/tax-authority/filings/f-1/poll"} {
		w := call(h, "POST", p, "tenant-a", nil)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "tax_authority_adapter_not_configured") {
			t.Fatalf("%s: want 503 adapter_not_configured, got %d: %s", p, w.Code, w.Body.String())
		}
	}
	// An unauthenticated callback is never accepted when nothing can verify it.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/tax-authority/callbacks/hmrc/tenant-a", strings.NewReader(`{"status":"ACCEPTED"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("callback without verifier: want 503, got %d: %s", w.Code, w.Body.String())
	}
}

// Without the adapter a filing is recorded PENDING — never ACCEPTED/SUBMITTED.
func TestFiling_WithoutAdapter_NeverClaimsAcceptance(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	tf := createIface(t, h, "tenant-a")
	w := call(h, "POST", "/v1/tax-authority/filings", "tenant-a", domain.SubmitTaxFilingRequest{
		InterfaceID: tf.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT_RETURN", TaxAmount: 10})
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"status":"PENDING"`) {
		t.Fatalf("want 202 PENDING, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthzDeny_403(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t, handler.ActionInterfaceCreate))
	w := call(h, "POST", "/v1/tax-authority/interfaces", "tenant-a", domain.CreateInterfaceRequest{
		LegalEntityID: "le-1", Jurisdiction: "GB", AuthorityName: "HMRC MTD"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied interface create: want 403, got %d: %s", w.Code, w.Body.String())
	}

	h = newRouter(t, pinger{}, policyAuthz(t, handler.ActionFilingSubmit))
	tf := createIface(t, h, "tenant-a") // allowed path through the same policy stub
	w = call(h, "POST", "/v1/tax-authority/filings", "tenant-a", domain.SubmitTaxFilingRequest{
		InterfaceID: tf.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT_RETURN"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied filing submit: want 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestValidation_400(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	if w := call(h, "POST", "/v1/tax-authority/interfaces", "tenant-a", map[string]string{"jurisdiction": "GB"}); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if w := call(h, "POST", "/v1/tax-authority/filings", "tenant-a", map[string]string{"interface_id": "x"}); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestMissingIdentity_FailsClosed(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/tax-authority/interfaces", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant: want 401, got %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/v1/tax-authority/interfaces", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("write without principal: want 401, got %d", w.Code)
	}
}

func TestCrossTenant_Denied(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	tf := createIface(t, h, "tenant-a")
	if w := call(h, "GET", "/v1/tax-authority/interfaces/"+tf.InterfaceID, "tenant-b", nil); w.Code != http.StatusNotFound {
		t.Fatalf("tenant-b read: want 404, got %d", w.Code)
	}
	if w := call(h, "POST", "/v1/tax-authority/filings", "tenant-b", domain.SubmitTaxFilingRequest{
		InterfaceID: tf.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT_RETURN"}); w.Code != http.StatusNotFound {
		t.Fatalf("tenant-b filing on tenant-a interface: want 404, got %d", w.Code)
	}
}

func TestIdempotentReplay_CreateInterface(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	body := `{"legal_entity_id":"le-1","jurisdiction":"GB","authority_name":"HMRC MTD"}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/tax-authority/interfaces", strings.NewReader(body))
		for k, v := range map[string]string{"X-Tenant-Id": "tenant-a", "X-Principal-Id": "p", "X-Request-Id": "r",
			"X-Correlation-ID": "c", "X-Source-Channel": "api", "X-Legal-Entity-Id": "le-1",
			"X-Purpose-Context": "tax-filing", "Idempotency-Key": "same-key"} {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	first, second := send(), send()
	if first.Code != http.StatusCreated || second.Header().Get("X-Idempotent-Replay") != "true" || first.Body.String() != second.Body.String() {
		t.Fatalf("replay: %d %d %q %s", first.Code, second.Code, second.Header().Get("X-Idempotent-Replay"), first.Body.String())
	}
	if w := call(h, "GET", "/v1/tax-authority/interfaces", "tenant-a", nil); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("replay must not create a second interface: %s", w.Body.String())
	}
}
