package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	svcenvelope "zoiko.io/connectivity-api-bridge-svc/internal/envelope"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

const (
	hRefAPI  = "0a000000-0000-4000-8000-0000000000c1"
	hAPIKey  = "handler-provider-api-key"
	hPayload = `{"amount":"10.00","currency":"GBP"}`
)

type hSecrets struct{}

func (hSecrets) Secret(_ context.Context, _ string, ref string) ([]byte, error) {
	if strings.EqualFold(ref, hRefAPI) {
		return []byte(hAPIKey), nil
	}
	return nil, outbound.ErrBlocked
}

type hConnAdapter struct{ svc *xicconn.Service }

func (c hConnAdapter) Connection(ctx context.Context, id string) (*xicconn.Connection, error) {
	return c.svc.Connection(ctx, id)
}
func (c hConnAdapter) PinnedContract(ctx context.Context, conn *xicconn.Connection) (*registry.ContractVersion, error) {
	return c.svc.PinnedContract(ctx, conn)
}
func (c hConnAdapter) Operable(ctx context.Context, tenant, id, op string) (*xicconn.Decision, error) {
	return c.svc.Operable(ctx, tenant, id, op)
}

type hProvider struct {
	mu    sync.Mutex
	posts int
	gets  int
	fn    func(method string) (int, string, error)
}

type hTimeout struct{}

func (hTimeout) Error() string   { return "i/o timeout" }
func (hTimeout) Timeout() bool   { return true }
func (hTimeout) Temporary() bool { return true }

func (p *hProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.ReadAll(r.Body)
	}
	p.mu.Lock()
	if r.Method == http.MethodPost {
		p.posts++
	} else {
		p.gets++
	}
	fn := p.fn
	p.mu.Unlock()
	status, body, err := 200, `{"payment_id":"P-1"}`, error(nil)
	if fn != nil {
		status, body, err = fn(r.Method)
	}
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader([]byte(body))), Request: r}, nil
}

func (p *hProvider) set(fn func(method string) (int, string, error)) {
	p.mu.Lock()
	p.fn = fn
	p.mu.Unlock()
}
func (p *hProvider) counts() (int, int) { p.mu.Lock(); defer p.mu.Unlock(); return p.posts, p.gets }

type dnsFlip struct {
	mu sync.Mutex
	ip string
}

func (d *dnsFlip) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return []net.IPAddr{{IP: net.ParseIP(d.ip)}}, nil
}

type obHarness struct {
	*connHarness
	svc  *outbound.Service
	repo *outbound.MemRepo
	or   chi.Router
	prov *hProvider
	dns  *dnsFlip
	conn xicconn.Connection
}

func newObHarness(t *testing.T) *obHarness {
	t.Helper()
	ch := newConnHarness(t)
	in := registrytest.Input("1.1.0")
	in.Content.Profiles = append(in.Content.Profiles, registry.Profile{Kind: registry.ProfileAuth, Name: "api-out",
		Spec: json.RawMessage(`{"mechanism":"API_KEY_HEADER","credential_class":"PROVIDER_CREDENTIAL","api_key_header":"X-Api-Key"}`)})
	for i := range in.Content.Operations {
		o := &in.Content.Operations[i]
		switch o.ID {
		case "payment-submit":
			o.AuthProfile, o.CallbackProfile, o.DataClasses, o.ResubmitAfterUnknown = "api-out", "", []string{"FINANCIAL"}, false
			o.UnknownResolution = registry.UnknownResolution{StatusQueryOperation: "payment-status"}
		case "payment-status":
			o.AuthProfile = "api-out"
		}
	}
	if w := ch.call("POST", "/v1/connectors/bank-x/versions", platformTenant, "author", in); w.Code != http.StatusCreated {
		t.Fatalf("create variant: %d %s", w.Code, w.Body.String())
	}
	v := ch.publishVariant(t, "bank-x", in)

	srv := httptest.NewServer(ch.authz.handler())
	t.Cleanup(srv.Close)
	h := &obHarness{connHarness: ch, repo: outbound.NewMemRepo(), prov: &hProvider{}, dns: &dnsFlip{ip: "93.184.216.34"}}
	clock := func() time.Time { return registrytest.T0.Add(72 * time.Hour) }
	h.svc = &outbound.Service{Repo: h.repo, Conns: hConnAdapter{ch.svc}, Secrets: hSecrets{}, Egress: egress.Policy{Resolver: h.dns},
		Cfg: outbound.DefaultConfig(), Clock: clock, HTTP: &http.Client{Transport: h.prov}}
	api := chi.NewRouter()
	api.Use(middleware.TenantContext)
	RegisterOutboundRoutes(api, NewOutbound(h.svc, h.repo, authz.NewClient(srv.URL), zap.NewNop()))
	h.or = api

	// an ACTIVE connection pinned to the outbound-capable variant
	c := ch.createFor(t, hTenantA, v, "acc-out")
	w := ch.cmd(t, c.ConnectionID, "authorize", hTenantA, "creator", map[string]any{"expected_version": c.Version, "mode": "PRE_PROVISIONED"}, 200)
	ver := version(t, w)
	exp := registrytest.T0.Add(200 * time.Hour).Format(time.RFC3339)
	w = ch.cmd(t, c.ConnectionID, "bind", hTenantA, "creator", map[string]any{"expected_version": ver, "bind": map[string]any{
		"auth_profile": "api-out", "credential_ref": hRefAPI, "consent_actor": "tenant-admin", "expires_at": exp}}, 200)
	ver = version(t, w)
	w = ch.cmd(t, c.ConnectionID, "bind", hTenantA, "creator", map[string]any{"expected_version": ver, "bind": map[string]any{
		"auth_profile": "hmac-in", "credential_ref": hRefHMAC, "consent_actor": "tenant-admin"}}, 200)
	ver = version(t, w)
	w = ch.cmd(t, c.ConnectionID, "validate", hTenantA, "operator", map[string]any{"expected_version": ver}, 200)
	ver = version(t, w)
	ch.cmd(t, c.ConnectionID, "activate", hTenantA, "operator", map[string]any{"expected_version": ver}, 200)
	h.conn = ch.get(t, hTenantA, c.ConnectionID)
	return h
}

func (h *connHarness) createFor(t *testing.T, tenant string, v registry.ContractVersion, account string) xicconn.Connection {
	t.Helper()
	w := h.c("POST", "/v1/integration-connections", tenant, "creator", map[string]any{"environment": "SANDBOX", "connector_id": "bank-x",
		"contract_version_id": v.ContractVersionID, "region": "uk", "endpoints": map[string]string{"bank-api": "api.bank-x.com"}, "provider_account_ref": account})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var c xicconn.Connection
	decodeInto(t, w, &c)
	return c
}

func (h *obHarness) o(method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
	return h.do(h.or, method, path, tenant, principal, body, hdr...)
}

func (h *obHarness) reserve(t *testing.T, ref string) outbound.Attempt {
	t.Helper()
	w := h.o("POST", "/v1/outbound-attempts", hTenantA, "domain-svc", map[string]any{"connection_id": h.conn.ConnectionID, "operation_id": "payment-submit",
		"domain_request_ref": ref, "payload": json.RawMessage(hPayload)})
	if w.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", w.Code, w.Body.String())
	}
	var a outbound.Attempt
	decodeInto(t, w, &a)
	return a
}

func (h *obHarness) cmdO(t *testing.T, id, cmd, tenant, principal string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := h.o("POST", "/v1/outbound-attempts/"+id+":"+cmd, tenant, principal, body)
	if w.Code != want {
		t.Fatalf("%s as %s: %d (want %d) %s", cmd, principal, w.Code, want, w.Body.String())
	}
	return w
}

// ---- reserve ---------------------------------------------------------------------------------------------------

func TestOutboundHTTP_ReserveIsDurableIdempotentAndNeverEchoesThePayload(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	if a.State != outbound.StateReserved || a.PayloadHash == "" || !a.BodyRetained || a.TenantID != hTenantA {
		t.Fatalf("%+v", a)
	}
	if posts, gets := h.prov.counts(); posts+gets != 0 {
		t.Fatal("reserving called the provider")
	}
	w := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "viewer", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	decodeInto(t, w, &raw)
	for _, k := range []string{"Body", "body", "payload"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("the attempt view exposes %s", k)
		}
	}
	if strings.Contains(w.Body.String(), "10.00") || strings.Contains(w.Body.String(), hAPIKey) {
		t.Fatalf("a payload value or a secret is in the view: %s", w.Body.String())
	}
	// the same domain request again: 200 and the original
	again := h.o("POST", "/v1/outbound-attempts", hTenantA, "domain-svc", map[string]any{"connection_id": h.conn.ConnectionID, "operation_id": "payment-submit",
		"domain_request_ref": "PAY-1", "payload": json.RawMessage(hPayload)})
	var a2 outbound.Attempt
	decodeInto(t, again, &a2)
	if again.Code != 200 || a2.AttemptID != a.AttemptID {
		t.Fatalf("%d %s", again.Code, again.Body.String())
	}
	// a different payload for it: a conflict
	p := isProblem(t, h.o("POST", "/v1/outbound-attempts", hTenantA, "domain-svc", map[string]any{"connection_id": h.conn.ConnectionID, "operation_id": "payment-submit",
		"domain_request_ref": "PAY-1", "payload": json.RawMessage(`{"amount":"99.00","currency":"GBP"}`)}), 409)
	if strings.Contains(p["detail"].(string), "99.00") {
		t.Fatalf("%v", p)
	}
	if l := h.o("GET", "/v1/outbound-attempts?state=RESERVED", hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":1`) {
		t.Fatalf("%s", l.Body.String())
	}
	if l := h.o("GET", "/v1/outbound-attempts?state=SETTLED", hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":0`) {
		t.Fatalf("%s", l.Body.String())
	}
	if l := h.o("GET", "/v1/outbound-attempts?domain_request_ref=PAY-1", hTenantA, "viewer", nil); !strings.Contains(l.Body.String(), `"total":1`) {
		t.Fatalf("%s", l.Body.String())
	}
}

func TestOutboundHTTP_ReserveRefusalsAreStableProblems(t *testing.T) {
	h := newObHarness(t)
	ok := map[string]any{"connection_id": h.conn.ConnectionID, "operation_id": "payment-submit", "domain_request_ref": "PAY-X", "payload": json.RawMessage(hPayload)}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range ok {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	for name, body := range map[string]map[string]any{
		"no payload":        with("payload", nil),
		"schema violation":  with("payload", json.RawMessage(`{"amount":"1.00"}`)),
		"bad request ref":   with("domain_request_ref", "has space"),
		"bad connection id": with("connection_id", "x"),
		"inbound operation": with("operation_id", "statement-push"),
	} {
		p := isProblem(t, h.o("POST", "/v1/outbound-attempts", hTenantA, "domain-svc", body), 422)
		if _, ok := p["retryable"]; !ok && name == "" {
			t.Error(p)
		}
	}
	// an unregistered operation: the contract refusal XIC-002
	p := isProblem(t, h.o("POST", "/v1/outbound-attempts", hTenantA, "domain-svc", with("operation_id", "nope")), 422)
	if p["code"] != "XIC-002" {
		t.Fatalf("%v", p)
	}
	// another tenant cannot reach this connection; a security signal-bearing refusal, never a success
	w := h.o("POST", "/v1/outbound-attempts", hTenantB, "domain-svc", ok)
	if w.Code < 400 || !strings.Contains(w.Body.String(), "XIC-008") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if l, _ := h.repo.List(middleware.WithTenant(context.Background(), hTenantA), outbound.Filter{}); len(l) != 0 {
		t.Fatalf("%d attempts left behind by refusals", len(l))
	}
	isProblem(t, h.o("POST", "/v1/outbound-attempts", hTenantA, "domain-svc", []byte(`{nope`)), 400)
}

// ---- submit, ambiguity and resolution ----------------------------------------------------------------------

func TestOutboundHTTP_SubmitSettlesAndIsIdempotent(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	w := h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", map[string]any{}, 200)
	var got outbound.Attempt
	decodeInto(t, w, &got)
	if got.State != outbound.StateSettled || got.ProviderReference != "P-1" || got.SubmitCount != 1 || got.BodyRetained {
		t.Fatalf("%+v", got)
	}
	// submitting again answers with the final state and sends nothing
	h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", nil, 200)
	if posts, _ := h.prov.counts(); posts != 1 {
		t.Fatalf("%d submissions", posts)
	}
	ev := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID+"/events", hTenantA, "viewer", nil)
	if ev.Code != 200 || !strings.Contains(ev.Body.String(), "RESERVED") || !strings.Contains(ev.Body.String(), "SUBMIT_STARTED") || !strings.Contains(ev.Body.String(), "RESOLVED") {
		t.Fatalf("%d %s", ev.Code, ev.Body.String())
	}
	if strings.Contains(ev.Body.String(), hAPIKey) || strings.Contains(ev.Body.String(), "10.00") {
		t.Fatal("evidence leaks")
	}
}

func TestOutboundHTTP_UnknownIsResolvedNeverResubmitted(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	h.prov.set(func(string) (int, string, error) { return 0, "", hTimeout{} })
	w := h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", nil, 200)
	var got outbound.Attempt
	decodeInto(t, w, &got)
	if got.State != outbound.StateUnknown || got.AmbiguityReason != outbound.ReasonTimeout {
		t.Fatalf("%+v", got)
	}
	// a second submit is a conflict: the original is to be resolved (I-14)
	for i := 0; i < 2; i++ {
		isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":submit", hTenantA, "domain-svc", map[string]any{}), 409)
	}
	if posts, _ := h.prov.counts(); posts != 1 {
		t.Fatalf("%d submissions of one logical operation", posts)
	}
	// resolve by query
	h.prov.set(func(m string) (int, string, error) {
		if m == http.MethodGet {
			return 200, `{"status":"SETTLED","payment_id":"P-4"}`, nil
		}
		return 200, `{}`, nil
	})
	w = h.cmdO(t, a.AttemptID, "resolve", hTenantA, "ops-lead", map[string]any{}, 200)
	decodeInto(t, w, &got)
	if got.State != outbound.StateSettled || got.ResolutionMethod != outbound.MethodStatusQuery || got.ProviderReference != "P-4" {
		t.Fatalf("%+v", got)
	}
	if posts, gets := h.prov.counts(); posts != 1 || gets != 1 {
		t.Fatalf("posts=%d gets=%d", posts, gets)
	}
}

func TestOutboundHTTP_ManualResolutionIsGoverned(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	h.prov.set(func(string) (int, string, error) { return 503, `{}`, nil })
	h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", nil, 200)
	manual := func(who string, body map[string]any, want int) {
		t.Helper()
		body["method"] = "MANUAL"
		h.cmdO(t, a.AttemptID, "resolve", hTenantA, who, body, want)
	}
	manual("ops-lead", map[string]any{"outcome": "SETTLED", "reason": "statement line 14"}, 422)                           // no evidence
	manual("domain-svc", map[string]any{"outcome": "SETTLED", "evidence_ref": "BS-1", "reason": "statement line 14"}, 403) // the requester
	manual("ops-lead", map[string]any{"outcome": "MAYBE", "evidence_ref": "BS-1", "reason": "x"}, 422)                     // outcome
	manual("ops-lead", map[string]any{"outcome": "FAILED", "evidence_ref": "BS-1", "reason": "statement shows no debit"}, 200)
	w := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "viewer", nil)
	if !strings.Contains(w.Body.String(), `"state":"FAILED"`) || !strings.Contains(w.Body.String(), `"resolution_method":"MANUAL"`) || !strings.Contains(w.Body.String(), `"resolved_by":"ops-lead"`) {
		t.Fatalf("%s", w.Body.String())
	}
	h.cmdO(t, a.AttemptID, "resolve", hTenantA, "ops-lead", map[string]any{"method": "BOGUS"}, 200) // a final attempt answers with its state
	// an unknown method on an open attempt is refused
	b := h.reserve(t, "PAY-2")
	h.prov.set(func(string) (int, string, error) { return 0, "", hTimeout{} })
	h.cmdO(t, b.AttemptID, "submit", hTenantA, "domain-svc", nil, 200)
	isProblem(t, h.o("POST", "/v1/outbound-attempts/"+b.AttemptID+":resolve", hTenantA, "ops-lead", map[string]any{"method": "BOGUS"}), 422)
}

func TestOutboundHTTP_BlockedDestinationIsXIC009AndNothingIsSent(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	h.dns.mu.Lock()
	h.dns.ip = "10.0.0.5" // DNS rebinding to a private address
	h.dns.mu.Unlock()
	p := isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":submit", hTenantA, "domain-svc", map[string]any{}), 403)
	if p["code"] != "XIC-009" {
		t.Fatalf("%v", p)
	}
	if posts, gets := h.prov.counts(); posts+gets != 0 {
		t.Fatal("a call was made to a refused destination")
	}
	w := h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "viewer", nil)
	if !strings.Contains(w.Body.String(), `"state":"RESERVED"`) || !strings.Contains(w.Body.String(), `"submit_count":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	h.dns.mu.Lock()
	h.dns.ip = "93.184.216.34"
	h.dns.mu.Unlock()
	h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", map[string]any{}, 200)
}

func TestOutboundHTTP_CancelOnlyBeforeAnySubmission(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":cancel", hTenantA, "ops-lead", map[string]any{"reason": ""}), 422)
	h.cmdO(t, a.AttemptID, "cancel", hTenantA, "ops-lead", map[string]any{"reason": "customer withdrew"}, 200)
	b := h.reserve(t, "PAY-2")
	h.prov.set(func(string) (int, string, error) { return 0, "", hTimeout{} })
	h.cmdO(t, b.AttemptID, "submit", hTenantA, "domain-svc", nil, 200)
	isProblem(t, h.o("POST", "/v1/outbound-attempts/"+b.AttemptID+":cancel", hTenantA, "ops-lead", map[string]any{"reason": "changed mind"}), 409)
}

func TestOutboundHTTP_BodyNotStoredNeedsThePayloadAgain(t *testing.T) {
	h := newObHarness(t)
	// a PERSONAL payload class is never stored: reserve with a connection whose operation carries it
	cfg := h.svc.Cfg
	cfg.BodyRetention = 0
	h.svc.Cfg = cfg
	a := h.reserve(t, "PAY-1")
	if a.BodyRetained {
		t.Fatal("stored")
	}
	isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":submit", hTenantA, "domain-svc", map[string]any{}), 422)
	isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":submit", hTenantA, "domain-svc", map[string]any{"payload": json.RawMessage(`{"amount":"11.00","currency":"GBP"}`)}), 422)
	if posts, _ := h.prov.counts(); posts != 0 {
		t.Fatal("something was sent")
	}
	h.cmdO(t, a.AttemptID, "submit", hTenantA, "domain-svc", map[string]any{"payload": json.RawMessage(hPayload)}, 200)
}

// ---- access control ----------------------------------------------------------------------------------------

func TestOutboundHTTP_TenantIsolationAndAuthorization(t *testing.T) {
	h := newObHarness(t)
	a := h.reserve(t, "PAY-1")
	for _, p := range []string{"", "/events"} {
		isProblem(t, h.o("GET", "/v1/outbound-attempts/"+a.AttemptID+p, hTenantB, "viewer", nil), 404)
	}
	if l := h.o("GET", "/v1/outbound-attempts", hTenantB, "viewer", nil); !strings.Contains(l.Body.String(), `"total":0`) {
		t.Fatalf("%s", l.Body.String())
	}
	for _, c := range []string{"submit", "resolve", "cancel"} {
		isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":"+c, hTenantB, "domain-svc", map[string]any{"reason": "x"}), 404)
	}
	if posts, gets := h.prov.counts(); posts+gets != 0 {
		t.Fatal("another tenant caused a call")
	}
	for action, call := range map[string]func() *httptest.ResponseRecorder{
		"XIC_OUTBOUND_READ": func() *httptest.ResponseRecorder {
			return h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "intruder", nil)
		},
		"XIC_OUTBOUND_RESERVE": func() *httptest.ResponseRecorder {
			return h.o("POST", "/v1/outbound-attempts", hTenantA, "intruder", map[string]any{"connection_id": h.conn.ConnectionID, "operation_id": "payment-submit", "domain_request_ref": "Z", "payload": json.RawMessage(hPayload)})
		},
		"XIC_OUTBOUND_SUBMIT": func() *httptest.ResponseRecorder {
			return h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":submit", hTenantA, "intruder", map[string]any{})
		},
		"XIC_OUTBOUND_RESOLVE": func() *httptest.ResponseRecorder {
			return h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":resolve", hTenantA, "intruder", map[string]any{})
		},
		"XIC_OUTBOUND_CANCEL": func() *httptest.ResponseRecorder {
			return h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":cancel", hTenantA, "intruder", map[string]any{"reason": "x"})
		},
	} {
		h.authz.deny("intruder", action)
		isProblem(t, call(), 403)
	}
	isProblem(t, h.o("GET", "/v1/outbound-attempts/"+a.AttemptID, hTenantA, "", nil), 401)
	isProblem(t, h.o("POST", "/v1/outbound-attempts/"+a.AttemptID+":explode", hTenantA, "domain-svc", map[string]any{}), 404)
	isProblem(t, h.o("GET", "/v1/outbound-attempts/not-a-uuid", hTenantA, "viewer", nil), 404)
	if posts, gets := h.prov.counts(); posts+gets != 0 {
		t.Fatal("a denied caller caused a call")
	}
	_ = errors.New
}

func TestOutboundHTTP_EnvelopeIsEnforcedOnWrites(t *testing.T) {
	h := newObHarness(t)
	srv := httptest.NewServer(h.authz.handler())
	defer srv.Close()
	strict := chi.NewRouter()
	strict.Use(middleware.TenantContext)
	strict.Use(newStrictEnvelope())
	RegisterOutboundRoutes(strict, NewOutbound(h.svc, h.repo, authz.NewClient(srv.URL), zap.NewNop()))
	body := map[string]any{"connection_id": h.conn.ConnectionID, "operation_id": "payment-submit", "domain_request_ref": "PAY-ENV", "payload": json.RawMessage(hPayload)}
	// without the canonical envelope headers a write is refused before any business logic
	if w := h.do(strict, "POST", "/v1/outbound-attempts", hTenantA, "domain-svc", body); w.Code < 400 {
		t.Fatalf("a write without the envelope was accepted: %d %s", w.Code, w.Body.String())
	}
	if l, _ := h.repo.List(middleware.WithTenant(context.Background(), hTenantA), outbound.Filter{}); len(l) != 0 {
		t.Fatal("an attempt was reserved without the envelope")
	}
}

func newStrictEnvelope() func(http.Handler) http.Handler {
	return svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeStrict, nil)
}
