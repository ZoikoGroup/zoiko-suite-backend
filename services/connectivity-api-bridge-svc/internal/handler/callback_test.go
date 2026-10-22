package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
)

type cbHarness struct {
	*obHarness
	root chi.Router
	at   time.Time
}

func newCBHarness(t *testing.T) *cbHarness {
	t.Helper()
	h := &cbHarness{obHarness: newObHarnessWith(t, true), at: registrytest.T0.Add(72 * time.Hour)}
	srv := httptest.NewServer(h.authz.handler())
	t.Cleanup(srv.Close)
	api := chi.NewRouter()
	api.Use(middleware.TenantContext)
	RegisterCallbackAdminRoutes(api, NewCallbackAdmin(h.repo, authz.NewClient(srv.URL), zap.NewNop()))
	RegisterOutboundRoutes(api, NewOutbound(h.svc, h.repo, authz.NewClient(srv.URL), zap.NewNop()))
	h.root = chi.NewRouter()
	h.root.Mount("/v1/callbacks", NewCallback(h.svc, 1<<20, false, zap.NewNop()).Router())
	h.root.Mount("/", api)
	return h
}

func cbEvent(event, ref, status, clientRef string) string {
	return `{"event_id":"` + event + `","payment_id":"` + ref + `","status":"` + status + `","client_ref":"` + clientRef + `"}`
}

// post delivers a callback exactly as a provider would: no tenant, no principal, no idempotency header.
func (h *cbHarness) post(connID, body, secret string, hdr ...string) *httptest.ResponseRecorder {
	if secret == "" {
		secret = hCBSecret
	}
	ts := strconv.FormatInt(h.at.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	req := httptest.NewRequest("POST", "/v1/callbacks/bank-x/"+connID+"/payment-submit", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", hex.EncodeToString(m.Sum(nil)))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	req.RemoteAddr = "198.51.100.9:5555"
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	return w
}

func (h *cbHarness) pendingAttempt(t *testing.T, ref string) outbound.Attempt {
	t.Helper()
	a := h.reserve(t, ref)
	h.prov.set(func(string) (int, string, error) { return 202, `{"payment_id":"P-9"}`, nil })
	w := h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", map[string]any{}, 200)
	var got outbound.Attempt
	decodeInto(t, w, &got)
	if got.State != outbound.StatePending {
		t.Fatalf("%+v", got)
	}
	return got
}

var traceMembers = regexp.MustCompile(`"(trace_id|instance|correlation_id)":"[^"]*",?`)

func TestCallbackHTTP_AppliedDuplicateAndTheReceiptsAPI(t *testing.T) {
	h := newCBHarness(t)
	a := h.pendingAttempt(t, "PAY-1")
	w := h.post(h.conn.ConnectionID, cbEvent("evt-1", "P-9", "SETTLED", "PAY-1"), "")
	var ok map[string]any
	decodeInto(t, w, &ok)
	if w.Code != 200 || ok["state"] != "APPLIED" || ok["duplicate"] != false || ok["receipt_id"] == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SETTLED") && strings.Contains(w.Body.String(), "P-9") {
		t.Fatalf("the acknowledgement echoes payload values: %s", w.Body.String())
	}
	// the attempt, as the tenant reads it
	g := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "viewer", nil)
	if !strings.Contains(g.Body.String(), `"state":"SETTLED"`) || !strings.Contains(g.Body.String(), `"resolution_method":"CALLBACK"`) {
		t.Fatalf("%s", g.Body.String())
	}
	// a repeat: 200 with the original receipt
	d := h.post(h.conn.ConnectionID, cbEvent("evt-1", "P-9", "SETTLED", "PAY-1"), "")
	var dup map[string]any
	decodeInto(t, d, &dup)
	if d.Code != 200 || dup["duplicate"] != true || dup["receipt_id"] != ok["receipt_id"] {
		t.Fatalf("%d %s", d.Code, d.Body.String())
	}
	// the receipts API: tenant-scoped evidence, no bodies
	rid := ok["receipt_id"].(string)
	got := h.o("GET", "/v1/callback-receipts/"+rid, hTenantA, "viewer", nil)
	var rc outbound.Receipt
	decodeInto(t, got, &rc)
	if got.Code != 200 || rc.State != "APPLIED" || rc.AttemptStateBefore != "PENDING" || rc.AttemptStateAfter != "SETTLED" || rc.BodyHash == "" {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
	if strings.Contains(got.Body.String(), "evt-1") {
		t.Fatalf("a receipt carries the event id: %s", got.Body.String())
	}
	if l := h.o("GET", "/v1/callback-receipts?state=APPLIED", hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":1`) {
		t.Fatalf("%s", l.Body.String())
	}
	if l := h.o("GET", "/v1/callback-receipts?state=DUPLICATE&attempt_id="+a.AttemptID, hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":1`) {
		t.Fatalf("%s", l.Body.String())
	}
	if l := h.o("GET", "/v1/callback-receipts?needs_reconciliation=true", hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":0`) {
		t.Fatalf("%s", l.Body.String())
	}
	// access control and isolation
	isProblem(t, h.o("GET", "/v1/callback-receipts/"+rid, hTenantB, "viewer", nil), 404)
	if l := h.o("GET", "/v1/callback-receipts", hTenantB, "viewer", nil); !strings.Contains(l.Body.String(), `"total":0`) {
		t.Fatalf("%s", l.Body.String())
	}
	h.authz.deny("intruder", "XIC_CALLBACK_READ")
	isProblem(t, h.o("GET", "/v1/callback-receipts/"+rid, hTenantA, "intruder", nil), 403)
	isProblem(t, h.o("GET", "/v1/callback-receipts", hTenantA, "", nil), 401)
	isProblem(t, h.o("GET", "/v1/callback-receipts/not-a-uuid", hTenantA, "viewer", nil), 404)
	req := httptest.NewRequest("GET", "/v1/callback-receipts", nil)
	rec := httptest.NewRecorder()
	h.root.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("the management API must still demand a tenant: %d", rec.Code)
	}
}

func TestCallbackHTTP_AuthenticationFailureIsUniformAndTheEndpointNeedsNoTenantHeaders(t *testing.T) {
	h := newCBHarness(t)
	h.pendingAttempt(t, "PAY-1")
	bodies := map[string]string{}
	post := func(path, body, secret string, hdr ...string) *httptest.ResponseRecorder {
		ts := strconv.FormatInt(h.at.Unix(), 10)
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(ts + "." + body))
		req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", hex.EncodeToString(m.Sum(nil)))
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.root.ServeHTTP(w, req)
		return w
	}
	good := cbEvent("e1", "P-9", "SETTLED", "PAY-1")
	base := "/v1/callbacks/bank-x/" + h.conn.ConnectionID + "/payment-submit"
	for name, w := range map[string]*httptest.ResponseRecorder{
		"bad secret":         post(base, good, "wrong"),
		"forged signature":   post(base, good, hCBSecret, "X-Signature", "deadbeef"),
		"no signature":       post(base, good, hCBSecret, "X-Signature", ""),
		"unknown connection": post("/v1/callbacks/bank-x/00000000-0000-4000-8000-000000000000/payment-submit", good, hCBSecret),
		"wrong connector":    post("/v1/callbacks/bank-y/"+h.conn.ConnectionID+"/payment-submit", good, hCBSecret),
		"malformed id":       post("/v1/callbacks/bank-x/x/payment-submit", good, hCBSecret),
		"inbound operation":  post("/v1/callbacks/bank-x/"+h.conn.ConnectionID+"/statement-push", good, hCBSecret),
		"status operation":   post("/v1/callbacks/bank-x/"+h.conn.ConnectionID+"/payment-status", good, hCBSecret),
	} {
		p := isProblem(t, w, 401)
		if p["code"] != "XIC-004" || p["receipt_id"] != nil {
			t.Errorf("%s: %v", name, p)
		}
		bodies[name] = traceMembers.ReplaceAllString(w.Body.String(), "")
	}
	ref := bodies["bad secret"]
	for name, b := range bodies {
		var x, y map[string]any
		_ = json.Unmarshal([]byte(b), &x)
		_ = json.Unmarshal([]byte(ref), &y)
		delete(x, "instance")
		delete(y, "instance")
		xb, _ := json.Marshal(x)
		yb, _ := json.Marshal(y)
		if string(xb) != string(yb) {
			t.Errorf("%s is distinguishable from a bad secret:\n%s\n%s", name, xb, yb)
		}
	}
	// only POST is routed
	for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(m, base, nil)
		w := httptest.NewRecorder()
		h.root.ServeHTTP(w, req)
		if w.Code != 405 && w.Code != 404 {
			t.Errorf("%s: %d", m, w.Code)
		}
	}
}

func TestCallbackHTTP_RefusalsAreStableProblemsWithTheReceipt(t *testing.T) {
	h := newCBHarness(t)
	a := h.reserve(t, "PAY-RES") // RESERVED: no callback can settle it
	w := h.post(h.conn.ConnectionID, cbEvent("e1", "P-1", "SETTLED", "PAY-RES"), "")
	p := isProblem(t, w, 409)
	if p["code"] != "XIC-011" || p["reason_code"] != "TRANSITION_NOT_ALLOWED" || p["receipt_id"] == nil || p["retryable"] != false {
		t.Fatalf("%v", p)
	}
	if strings.Contains(w.Body.String(), "SETTLED") || strings.Contains(w.Body.String(), "P-1") {
		t.Fatalf("the problem echoes payload values: %s", w.Body.String())
	}
	// not matched yet: 409 with Retry-After so the provider retries
	w = h.post(h.conn.ConnectionID, `{"event_id":"e2","payment_id":"P-404","status":"SETTLED"}`, "")
	p = isProblem(t, w, 409)
	if p["reason_code"] != "ATTEMPT_NOT_FOUND" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%v %v", p, w.Header())
	}
	// shape: 422 / 415
	isProblem(t, h.post(h.conn.ConnectionID, `{"event_id":"e3","payment_id":"P-9"}`, ""), 422)
	isProblem(t, h.post(h.conn.ConnectionID, cbEvent("e4", "P-1", "SETTLED", "PAY-RES"), "", "Content-Type", "text/plain"), 415)
	p = isProblem(t, h.post(h.conn.ConnectionID, cbEvent("e5", "P-1", "SETTLED", "PAY-RES"), "", "X-Source-Schema-Version", "payment-cb@9"), 422)
	if p["code"] != "XIC-003" {
		t.Fatalf("%v", p)
	}
	p = isProblem(t, h.post(h.conn.ConnectionID, cbEvent("e6", "P-1", "HOLDING", "PAY-RES"), ""), 422)
	if p["code"] != "XIC-011" {
		t.Fatalf("%v", p)
	}
	// replay window: valid signature, stale timestamp
	h.at = h.at.Add(-3 * time.Hour)
	stale := h.post(h.conn.ConnectionID, cbEvent("e7", "P-1", "SETTLED", "PAY-RES"), "")
	h.at = h.at.Add(3 * time.Hour)
	p = isProblem(t, stale, 401)
	if p["code"] != "XIC-005" {
		t.Fatalf("%v", p)
	}
	// nothing moved, and the evidence is there to read
	g := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "viewer", nil)
	if !strings.Contains(g.Body.String(), `"state":"RESERVED"`) {
		t.Fatalf("%s", g.Body.String())
	}
	if l := h.o("GET", "/v1/callback-receipts?state=REJECTED", hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":7`) {
		t.Fatalf("%s", l.Body.String())
	}
}

func TestCallbackHTTP_LateContradictionIsFlaggedForReconciliation(t *testing.T) {
	h := newCBHarness(t)
	a := h.reserve(t, "PAY-1")
	h.prov.set(func(string) (int, string, error) { return 400, `{}`, nil })
	h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", map[string]any{}, 200) // FAILED
	p := isProblem(t, h.post(h.conn.ConnectionID, cbEvent("late-1", "P-3", "SETTLED", "PAY-1"), ""), 409)
	if p["reason_code"] != "LATE_CONTRADICTS_FINAL" {
		t.Fatalf("%v", p)
	}
	l := h.o("GET", "/v1/callback-receipts?needs_reconciliation=true", hTenantA, "viewer", nil)
	if !strings.Contains(l.Body.String(), `"total":1`) || !strings.Contains(l.Body.String(), `"needs_reconciliation":true`) {
		t.Fatalf("%s", l.Body.String())
	}
	g := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "viewer", nil)
	if !strings.Contains(g.Body.String(), `"state":"FAILED"`) {
		t.Fatalf("history was rewritten: %s", g.Body.String())
	}
}

func TestCallbackHTTP_SizeLimitsRefuseBeforeParsing(t *testing.T) {
	h := newCBHarness(t)
	h.pendingAttempt(t, "PAY-1")
	huge := `{"event_id":"e","pad":"` + strings.Repeat("a", 2<<20) + `"}`
	if w := h.post(h.conn.ConnectionID, huge, ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", w.Code)
	}
	req := httptest.NewRequest("POST", "/v1/callbacks/bank-x/"+h.conn.ConnectionID+"/payment-submit", strings.NewReader("{}"))
	req.ContentLength = 50 << 20
	rec := httptest.NewRecorder()
	h.root.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", rec.Code)
	}
	if n := len(h.repo.Types()); n == 0 {
		t.Fatal("expected the earlier flow's events")
	}
}

func TestCallbackHTTP_RateLimitIs429WithRetryAfter(t *testing.T) {
	h := newCBHarness(t)
	cfg := outbound.DefaultConfig()
	cfg.CallbackConnRatePerMin = 3
	h.svc.Cfg = cfg
	var limited *httptest.ResponseRecorder
	for i := 0; i < 10 && limited == nil; i++ {
		if w := h.post(h.conn.ConnectionID, cbEvent("r"+strconv.Itoa(i), "P-9", "SETTLED", "PAY-1"), "wrong"); w.Code == 429 {
			limited = w
		}
	}
	if limited == nil {
		t.Fatal("never limited")
	}
	p := isProblem(t, limited, 429)
	if p["code"] != "XIC-012" || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("%v", p)
	}
}

// o routes through the root router, which carries the receipts API as well as the attempts API.
func (h *cbHarness) o(method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
	return h.do(h.root, method, path, tenant, principal, body, hdr...)
}
