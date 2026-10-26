package handler

import (
	"bytes"
	"context"
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
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/integration"
)

const hmacSecret = "handler-test-secret"

type memSecrets struct{ m map[string]string }

func (s memSecrets) Secret(_ context.Context, _ string, ref string) ([]byte, error) {
	if v, ok := s.m[strings.ToLower(ref)]; ok {
		return []byte(v), nil
	}
	return nil, inbound.ErrAuthFailed
}

type inHarness struct {
	*connHarness
	gw     *inbound.Service
	irepo  *inbound.MemRepo
	root   chi.Router
	now    time.Time
	secret memSecrets
}

func newInHarness(t *testing.T) *inHarness {
	t.Helper()
	ch := newConnHarness(t)
	ir := inbound.NewMemRepo()
	h := &inHarness{connHarness: ch, irepo: ir, now: registrytest.T0.Add(72 * time.Hour), secret: memSecrets{m: map[string]string{strings.ToLower(hRefHMAC): hmacSecret}}}
	h.gw = &inbound.Service{Repo: ir, Conns: ch.svc, Secrets: h.secret, Cfg: inbound.DefaultConfig(),
		Clock:   func() time.Time { return h.now },
		Handoff: inbound.HandoffDeps{AllowedRegions: []string{"uk"}, Check: func(context.Context, string) (string, error) { return "ok", nil }}}
	srv := httptest.NewServer(ch.authz.handler())
	t.Cleanup(srv.Close)
	admin := NewInboundAdmin(h.gw, ir, authz.NewClient(srv.URL), zap.NewNop())
	api := chi.NewRouter()
	api.Use(middleware.TenantContext)
	RegisterInboundAdminRoutes(api, admin)
	RegisterConnectionRoutes(api, NewConnection(ch.svc, ch.conns, authz.NewClient(srv.URL), zap.NewNop()))
	root := chi.NewRouter()
	root.Mount("/v1/inbound", NewIngest(h.gw, 1<<20, false, zap.NewNop()).Router())
	root.Mount("/", api)
	h.root = root
	// the platform registers and approves the handoff target
	for _, st := range []struct{ who, path string }{{"platform-a", "create"}} {
		_ = st
	}
	w := h.adm("POST", "/v1/handoff-targets", platformTenant, "platform-a", map[string]any{"canonical_contract": "bank.statement.v1", "base_url": "https://domain.example", "path": "/v1/statements", "region": "uk"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create target: %d %s", w.Code, w.Body.String())
	}
	var tg inbound.Target
	decodeInto(t, w, &tg)
	w = h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":approve", platformTenant, "platform-b", map[string]any{"expected_version": tg.Version})
	if w.Code != 200 {
		t.Fatalf("approve target: %d %s", w.Code, w.Body.String())
	}
	return h
}

func (h *inHarness) adm(method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", principal)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	return w
}

func hsign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

// ingest posts a delivery to the public endpoint exactly as an external sender would: no tenant, no principal.
func (h *inHarness) ingest(c xicconn.Connection, body, key, secret string, hdr ...string) *httptest.ResponseRecorder {
	ts := strconv.FormatInt(h.now.Unix(), 10)
	req := httptest.NewRequest("POST", "/v1/inbound/"+c.ConnectorID+"/"+c.ConnectionID+"/statement-push", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", hsign(secret, ts, body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	req.RemoteAddr = "198.51.100.9:5555"
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	return w
}

func good(event string) string {
	return `{"event_id":"` + event + `","account":"A1","balance":"10.5","currency":"GBP","booked_at":"2026-10-20T10:00:00Z"}`
}

func (h *inHarness) conn(t *testing.T) xicconn.Connection {
	t.Helper()
	return h.active(t, hTenantA, "acc-ingest")
}

// ---- the public endpoint ------------------------------------------------------------------------

func TestIngestHTTP_AcceptedDuplicateAndProblemShapes(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	w := h.ingest(c, good("e1"), "k1", hmacSecret)
	var acc map[string]any
	decodeInto(t, w, &acc)
	if w.Code != 202 || acc["transport"] != "ACCEPTED" || acc["domain"] != "PENDING" || acc["state"] != "VALIDATED" || acc["exchange_id"] == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "A1") {
		t.Fatal("the response echoes a payload value")
	}
	// duplicate: 200 with the original reference
	w2 := h.ingest(c, good("e1"), "k1", hmacSecret)
	var dup map[string]any
	decodeInto(t, w2, &dup)
	if w2.Code != 200 || dup["duplicate"] != true || dup["exchange_id"] != acc["exchange_id"] {
		t.Fatalf("%d %s", w2.Code, w2.Body.String())
	}
	// changed body under the same key: 409 with the XIC code, the exchange id and no values
	w3 := h.ingest(c, strings.Replace(good("e1"), "10.5", "77.7", 1), "k1", hmacSecret)
	p := isProblem(t, w3, 409)
	if p["code"] != "XIC-006" || p["exchange_id"] == nil || strings.Contains(w3.Body.String(), "77.7") {
		t.Fatalf("%v", p)
	}
	// schema version the contract does not know: quarantined, XIC-003, 422
	w4 := h.ingest(c, good("e2"), "k2", hmacSecret, "X-Source-Schema-Version", "statement@9")
	p = isProblem(t, w4, 422)
	if p["code"] != "XIC-003" || p["state"] != "QUARANTINED" || p["reason_code"] != "SCHEMA_VERSION_UNSUPPORTED" {
		t.Fatalf("%v", p)
	}
	// a lossy decimal: XIC-007
	w5 := h.ingest(c, strings.Replace(good("e3"), "10.5", "10.555", 1), "k3", hmacSecret)
	p = isProblem(t, w5, 422)
	if p["code"] != "XIC-007" || p["reason_code"] != "PRECISION_LOSS" || strings.Contains(w5.Body.String(), "10.555") {
		t.Fatalf("%v", p)
	}
	// stable problem members only (§14.3)
	for _, k := range []string{"type", "title", "status", "code", "name", "retryable", "trace_id"} {
		if _, ok := p[k]; !ok && k != "trace_id" {
			t.Errorf("problem lacks %s", k)
		}
	}
}

func TestIngestHTTP_AuthenticationFailureIsUniformAndNeedsNoTenantHeaders(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	bodies := map[string]string{}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"bad secret":         h.ingest(c, good("e1"), "k1", "wrong"),
		"no signature":       h.ingest(c, good("e1"), "k1", hmacSecret, "X-Signature", ""),
		"unknown connection": h.ingest(xicconn.Connection{ConnectorID: "bank-x", ConnectionID: "00000000-0000-4000-8000-000000000000"}, good("e1"), "k1", hmacSecret),
		"wrong connector":    h.ingest(xicconn.Connection{ConnectorID: "bank-y", ConnectionID: c.ConnectionID}, good("e1"), "k1", hmacSecret),
		"malformed id":       h.ingest(xicconn.Connection{ConnectorID: "bank-x", ConnectionID: "x"}, good("e1"), "k1", hmacSecret),
	} {
		p := isProblem(t, w, 401)
		if p["code"] != "XIC-004" {
			t.Errorf("%s: %v", name, p)
		}
		b := w.Body.String()
		b = regexpTrace.ReplaceAllString(b, "")
		bodies[name] = b
	}
	ref := bodies["bad secret"]
	for name, b := range bodies {
		if strings.ReplaceAll(b, "\n", "") != strings.ReplaceAll(ref, "\n", "") && !strings.Contains(name, "instance") {
			// the instance path differs by design (it echoes the route); everything else must be identical
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
	}
	if len(h.irepo.Out) != 2 { // only the platform target events
		t.Fatalf("%d events from unauthenticated requests", len(h.irepo.Out)-2)
	}
	// the ingest route needs no tenant, but the management API still does
	req := httptest.NewRequest("GET", "/v1/integration-exchanges", nil)
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("the management API must still demand a tenant: %d", w.Code)
	}
	// only POST is routed
	for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(m, "/v1/inbound/bank-x/"+c.ConnectionID+"/statement-push", nil)
		w := httptest.NewRecorder()
		h.root.ServeHTTP(w, req)
		if w.Code != 405 && w.Code != 404 {
			t.Errorf("%s: %d", m, w.Code)
		}
	}
}

var regexpTrace = mustRegexp(`"trace_id":"[^"]*",?`)

func TestIngestHTTP_SizeLimitsRefuseBeforeParsing(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	huge := strings.Repeat("a", 2<<20)
	w := h.ingest(c, `{"event_id":"e","pad":"`+huge+`"}`, "k", hmacSecret)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d %s", w.Code, w.Body.String()[:120])
	}
	if h.irepo.Types()[len(h.irepo.Types())-1] == "integration.inbound.rejected" {
		t.Fatal("an oversized body was recorded as a delivery")
	}
	// Content-Length beyond the cap is refused without reading the body
	req := httptest.NewRequest("POST", "/v1/inbound/bank-x/"+c.ConnectionID+"/statement-push", strings.NewReader("{}"))
	req.ContentLength = 50 << 20
	rec := httptest.NewRecorder()
	h.root.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", rec.Code)
	}
}

func TestIngestHTTP_RateLimitIs429WithRetryAfterAndTheXICCode(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	h.gw.Cfg.ConnRatePerMin = 3
	var limited *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		w := h.ingest(c, good("e"+strconv.Itoa(i)), "k"+strconv.Itoa(i), "wrong")
		if w.Code == 429 {
			limited = w
			break
		}
	}
	if limited == nil {
		t.Fatal("never limited")
	}
	p := isProblem(t, limited, 429)
	if p["code"] != "XIC-012" || p["retryable"] != true || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("%v", p)
	}
}

func TestIngestHTTP_StorageOutageIs503WithRetryAfter(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	h.irepo.FailNext = true
	w := h.ingest(c, good("e1"), "k1", hmacSecret)
	isProblem(t, w, 503)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	if w := h.ingest(c, good("e1"), "k1", hmacSecret); w.Code != 202 {
		t.Fatalf("the retry must be accepted: %d", w.Code)
	}
}

func TestIngestHTTP_ForwardedForIsOnlyTrustedWhenConfigured(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	h.gw.Cfg.IPRatePerMin = 2
	// untrusted: a spoofed header does not buy a fresh budget
	limited := 0
	for i := 0; i < 8; i++ {
		w := h.ingest(c, good("e"), "k", "wrong", "X-Forwarded-For", "203.0.113."+strconv.Itoa(i))
		if w.Code == 429 {
			limited++
		}
	}
	if limited < 5 {
		t.Fatalf("a spoofed X-Forwarded-For escaped the limit (%d limited)", limited)
	}
}

// ---- management API -------------------------------------------------------------------------------

func TestInboundAdminHTTP_ExchangesNeverExposeBodies(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	var acc map[string]any
	decodeInto(t, h.ingest(c, good("e1"), "k1", hmacSecret), &acc)
	id := acc["exchange_id"].(string)
	w := h.adm("GET", "/v1/integration-exchanges/"+id, hTenantA, "viewer", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var x map[string]any
	decodeInto(t, w, &x)
	for _, k := range []string{"Raw", "raw", "Canonical", "canonical", "DedupeKey", "dedupe_key"} {
		if _, ok := x[k]; ok {
			t.Errorf("the exchange view exposes %s", k)
		}
	}
	if x["state"] != "VALIDATED" || x["domain_outcome"] != "PENDING" || x["raw_retained"] != true || x["canonical_hash"] == "" {
		t.Fatalf("%v", x)
	}
	if strings.Contains(w.Body.String(), `"A1"`) || strings.Contains(w.Body.String(), "10.50") {
		t.Fatal("a payload value is in the exchange view")
	}
	if w := h.adm("GET", "/v1/integration-exchanges?state=VALIDATED", hTenantA, "viewer", nil); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("%s", w.Body.String())
	}
	if w := h.adm("GET", "/v1/integration-exchanges?state=QUARANTINED", hTenantA, "viewer", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	for _, p := range []string{"events", "requests"} {
		if w := h.adm("GET", "/v1/integration-exchanges/"+id+"/"+p, hTenantA, "viewer", nil); w.Code != 200 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	// another tenant sees nothing, and gets a plain 404
	isProblem(t, h.adm("GET", "/v1/integration-exchanges/"+id, hTenantB, "viewer", nil), 404)
	for _, p := range []string{"/events", "/requests"} {
		isProblem(t, h.adm("GET", "/v1/integration-exchanges/"+id+p, hTenantB, "viewer", nil), 404)
	}
	if w := h.adm("GET", "/v1/integration-exchanges", hTenantB, "viewer", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	// authorization
	h.authz.deny("intruder", "XIC_INBOUND_READ")
	isProblem(t, h.adm("GET", "/v1/integration-exchanges/"+id, hTenantA, "intruder", nil), 403)
	isProblem(t, h.adm("GET", "/v1/integration-exchanges/"+id, hTenantA, "", nil), 401)
	isProblem(t, h.adm("GET", "/v1/integration-exchanges/not-a-uuid", hTenantA, "viewer", nil), 404)
}

func TestInboundAdminHTTP_QuarantineReleaseAndDismissal(t *testing.T) {
	h := newInHarness(t)
	c := h.conn(t)
	// a delivery held for a domain precondition (no target for a second contract is simulated by a lossy mapping)
	var q map[string]any
	decodeInto(t, h.ingest(c, strings.Replace(good("e1"), "10.5", "10.555", 1), "k1", hmacSecret), &q)
	id := q["exchange_id"].(string)
	// a mapping quarantine is releasable (raw is retained); request, self-approval is refused, another approves
	w := h.adm("POST", "/v1/integration-exchanges/"+id+"/requests", hTenantA, "requester", map[string]any{"action": "DISMISS", "reason": "sender confirmed the value is wrong"})
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var req inbound.QRequest
	decodeInto(t, w, &req)
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests/"+req.RequestID+":approve", hTenantA, "requester", nil), 403)
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests/not-there:approve", hTenantA, "approver", nil), 404)
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests/"+req.RequestID+":explode", hTenantA, "approver", nil), 404)
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests/"+req.RequestID+":approve", hTenantB, "approver", nil), 404)
	w = h.adm("POST", "/v1/integration-exchanges/"+id+"/requests/"+req.RequestID+":approve", hTenantA, "approver", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"REJECTED"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests/"+req.RequestID+":reject", hTenantA, "approver", nil), 409)
	// bad requests
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests", hTenantA, "requester", map[string]any{"action": "DELETE", "reason": "x"}), 422) // unknown action is a schema-level refusal
	isProblem(t, h.adm("POST", "/v1/integration-exchanges/"+id+"/requests", hTenantA, "requester", []byte(`{`)), 400)
	// an auth-context quarantine cannot be released
	var a map[string]any
	decodeInto(t, h.ingest(c, strings.Replace(`{"event_id":"e2","account":"A1","balance":"1","currency":"GBP","booked_at":"2026-10-20T10:00:00Z","tenant":"tenant-b"}`, "tenant-b", "tenant-b", 1), "k2", hmacSecret), &a)
	_ = a
	each := []string{"XIC_INBOUND_RELEASE_REQUEST", "XIC_INBOUND_RELEASE_DECIDE"}
	got := strings.Join(h.authz.actions, ",")
	for _, ac := range each {
		if !strings.Contains(got, ac) {
			t.Errorf("%s never asked", ac)
		}
	}
}

func TestInboundAdminHTTP_HandoffTargetsArePlatformOnlyWithMakerChecker(t *testing.T) {
	h := newInHarness(t)
	body := map[string]any{"canonical_contract": "tax.return.v1", "base_url": "https://tax.example", "path": "/v1/returns", "region": "uk"}
	// a tenant cannot register or even list targets
	isProblem(t, h.adm("POST", "/v1/handoff-targets", hTenantA, "tenant-admin", body), 403)
	isProblem(t, h.adm("GET", "/v1/handoff-targets", hTenantA, "tenant-admin", nil), 404)
	w := h.adm("POST", "/v1/handoff-targets", platformTenant, "author", body)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var tg inbound.Target
	decodeInto(t, w, &tg)
	isProblem(t, h.adm("POST", "/v1/handoff-targets", platformTenant, "author", body), 409) // one live target per contract
	isProblem(t, h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":approve", platformTenant, "author", map[string]any{"expected_version": tg.Version}), 403)
	isProblem(t, h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":approve", platformTenant, "approver", map[string]any{}), 400)
	isProblem(t, h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":approve", platformTenant, "approver", map[string]any{"expected_version": 9}), 409)
	isProblem(t, h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":explode", platformTenant, "approver", map[string]any{"expected_version": 1}), 404)
	isProblem(t, h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":approve", hTenantA, "approver", map[string]any{"expected_version": 1}), 403)
	if w := h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":approve", platformTenant, "approver", map[string]any{"expected_version": tg.Version}); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := h.adm("GET", "/v1/handoff-targets", platformTenant, "x", nil); !strings.Contains(w.Body.String(), "tax.return.v1") {
		t.Fatalf("%s", w.Body.String())
	}
	if w := h.adm("POST", "/v1/handoff-targets/"+tg.TargetID+":suspend", platformTenant, "approver", map[string]any{"expected_version": 2}); w.Code != 200 || !strings.Contains(w.Body.String(), "SUSPENDED") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.adm("POST", "/v1/handoff-targets", platformTenant, "author", map[string]any{"canonical_contract": "x", "base_url": "https://u:p@h.example", "path": "/x", "region": "uk"}), 422)
	h.authz.deny("nobody", "XIC_HANDOFF_TARGET_CREATE")
	isProblem(t, h.adm("POST", "/v1/handoff-targets", platformTenant, "nobody", map[string]any{"canonical_contract": "z.v1", "base_url": "https://z.example", "path": "/x", "region": "uk"}), 403)
}

var _ = integration.XICCode("")

func mustRegexp(p string) *regexp.Regexp { return regexp.MustCompile(p) }
