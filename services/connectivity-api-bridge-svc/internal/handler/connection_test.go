package handler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	svcenvelope "zoiko.io/connectivity-api-bridge-svc/internal/envelope"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

const (
	hRefOAuth = "0a000000-0000-4000-8000-0000000000a1"
	hRefHMAC  = "0a000000-0000-4000-8000-0000000000a2"
	hTenantA  = "tenant-a"
	hTenantB  = "tenant-b"
)

type hVault struct{}

func (hVault) Verify(context.Context, string, vault.Caller) error { return nil }

type hDNS struct{}

func (hDNS) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type connHarness struct {
	*regHarness
	cr      chi.Router
	cstrict chi.Router
	conns   *xicconn.MemRepo
	svc     *xicconn.Service
	v       registry.ContractVersion
}

func newConnHarness(t *testing.T) *connHarness {
	t.Helper()
	rh := newRegHarness(t)
	conns := xicconn.NewMemRepo()
	srv := httptest.NewServer(rh.authz.handler())
	t.Cleanup(srv.Close)
	svc := &xicconn.Service{Repo: conns, Contracts: rh.repo, Vault: hVault{}, Egress: egress.Policy{Resolver: hDNS{}},
		AllowedRegions: []string{"uk"}, RedirectAllowlist: []string{"https://app.zoiko.example/oauth/callback"},
		Clock: func() time.Time { return registrytest.T0.Add(72 * time.Hour) }}
	ch := NewConnection(svc, conns, authz.NewClient(srv.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterConnectionRoutes(r, ch)
	strict := chi.NewRouter()
	strict.Use(middleware.TenantContext)
	strict.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeStrict, nil))
	RegisterConnectionRoutes(strict, ch)
	h := &connHarness{regHarness: rh, cr: r, cstrict: strict, conns: conns, svc: svc}
	rh.connector(t, "bank-x")
	h.v = rh.publishedOverHTTP(t, "bank-x", "1.0.0")
	return h
}

func (h *connHarness) c(method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
	return h.do(h.cr, method, path, tenant, principal, body, hdr...)
}

func (h *connHarness) create(t *testing.T, tenant, env, account string) xicconn.Connection {
	t.Helper()
	w := h.c("POST", "/v1/integration-connections", tenant, "creator", map[string]any{"environment": env, "connector_id": "bank-x",
		"contract_version_id": h.v.ContractVersionID, "region": "uk", "endpoints": map[string]string{"bank-api": "api.bank-x.com"}, "provider_account_ref": account})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var c xicconn.Connection
	decodeInto(t, w, &c)
	return c
}

func (h *connHarness) cmd(t *testing.T, id, cmd, tenant, principal string, body map[string]any, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := h.c("POST", "/v1/integration-connections/"+id+":"+cmd, tenant, principal, body)
	if w.Code != want {
		t.Fatalf("%s as %s: %d (want %d) %s", cmd, principal, w.Code, want, w.Body.String())
	}
	return w
}

func version(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var c struct {
		Version    int64 `json:"version"`
		Connection *struct {
			Version int64 `json:"version"`
		} `json:"connection"`
	}
	decodeInto(t, w, &c)
	if c.Connection != nil {
		return c.Connection.Version
	}
	return c.Version
}

func (h *connHarness) get(t *testing.T, tenant, id string) xicconn.Connection {
	t.Helper()
	w := h.c("GET", "/v1/integration-connections/"+id, tenant, "x", nil)
	var c xicconn.Connection
	decodeInto(t, w, &c)
	return c
}

// active drives a sandbox connection to ACTIVE over HTTP and returns it.
func (h *connHarness) active(t *testing.T, tenant, account string) xicconn.Connection {
	t.Helper()
	c := h.create(t, tenant, "SANDBOX", account)
	w := h.cmd(t, c.ConnectionID, "authorize", tenant, "creator", map[string]any{"expected_version": c.Version, "mode": "PRE_PROVISIONED"}, 200)
	v := version(t, w)
	exp := registrytest.T0.Add(200 * time.Hour).Format(time.RFC3339)
	w = h.cmd(t, c.ConnectionID, "bind", tenant, "creator", map[string]any{"expected_version": v, "bind": map[string]any{
		"auth_profile": "oauth", "credential_ref": hRefOAuth, "granted_scopes": []string{"payments.write", "payments.read"}, "consent_actor": "tenant-admin", "expires_at": exp}}, 200)
	v = version(t, w)
	w = h.cmd(t, c.ConnectionID, "bind", tenant, "creator", map[string]any{"expected_version": v, "bind": map[string]any{
		"auth_profile": "hmac-in", "credential_ref": hRefHMAC, "consent_actor": "tenant-admin"}}, 200)
	v = version(t, w)
	w = h.cmd(t, c.ConnectionID, "validate", tenant, "operator", map[string]any{"expected_version": v}, 200)
	v = version(t, w)
	h.cmd(t, c.ConnectionID, "activate", tenant, "operator", map[string]any{"expected_version": v}, 200)
	return h.get(t, tenant, c.ConnectionID)
}

// ---- lifecycle over HTTP ------------------------------------------------------------------

func TestConnectionHTTP_Lifecycle(t *testing.T) {
	h := newConnHarness(t)
	c := h.active(t, hTenantA, "acc-1")
	if c.Status != xicconn.StatusActive || len(c.Bindings) != 2 || c.ActivatedBy != "operator" {
		t.Fatalf("%+v", c)
	}
	// credential references are masked in what the API returns; the raw account never appears
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), hRefOAuth) || strings.Contains(string(raw), "acc-1") || !strings.Contains(string(raw), "…") {
		t.Fatalf("response exposes more than it should: %s", raw)
	}
	// operability
	w := h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/operability?operation=payment-submit", hTenantA, "x", nil)
	var op map[string]any
	decodeInto(t, w, &op)
	if w.Code != 200 || op["operable"] != true || op["host"] != "api.bank-x.com" || op["credential_bound"] != true {
		t.Fatalf("%d %v", w.Code, op)
	}
	if strings.Contains(w.Body.String(), hRefOAuth) {
		t.Fatal("operability returned a credential reference")
	}
	for _, p := range []string{"checks", "events", "requests", "certifications"} {
		if w := h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/"+p, hTenantA, "x", nil); w.Code != 200 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	if w := h.c("GET", "/v1/integration-connections?status=ACTIVE", hTenantA, "x", nil); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("%s", w.Body.String())
	}
	if w := h.c("GET", "/v1/integration-connections?status=SUSPENDED", hTenantA, "x", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	// suspend: operability is refused with XIC-001 from the next request
	h.cmd(t, c.ConnectionID, "suspend", hTenantA, "operator", map[string]any{"expected_version": c.Version, "reason": "PROVIDER_INCIDENT"}, 200)
	w = h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/operability?operation=payment-submit", hTenantA, "x", nil)
	p := isProblem(t, w, 409)
	if p["code"] != "XIC-001" || p["connection_id"] != c.ConnectionID {
		t.Fatalf("%v", p)
	}
	cur := h.get(t, hTenantA, c.ConnectionID)
	h.cmd(t, c.ConnectionID, "revoke", hTenantA, "operator", map[string]any{"expected_version": cur.Version, "reason": "decommissioned"}, 200)
	if h.get(t, hTenantA, c.ConnectionID).Status != xicconn.StatusRevoked {
		t.Fatal("not revoked")
	}
}

func TestConnectionHTTP_XICCodes(t *testing.T) {
	h := newConnHarness(t)
	c := h.active(t, hTenantA, "acc")
	// another tenant's connection: XIC-008, and 404 on every ordinary read
	w := h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/operability?operation=payment-submit", hTenantB, "x", nil)
	if p := isProblem(t, w, 404); p["code"] != nil && p["code"] != "XIC-008" {
		t.Fatalf("%v", p)
	}
	// an unregistered operation: XIC-002
	w = h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/operability?operation=drop-tables", hTenantA, "x", nil)
	if p := isProblem(t, w, 422); p["code"] != "XIC-002" {
		t.Fatalf("%v", p)
	}
	// no operation named
	isProblem(t, h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/operability", hTenantA, "x", nil), 422)
}

func TestConnectionHTTP_ProductionCertificationIsXIC017(t *testing.T) {
	h := newConnHarness(t)
	c := h.create(t, hTenantA, "PRODUCTION", "acc")
	w := h.cmd(t, c.ConnectionID, "authorize", hTenantA, "creator", map[string]any{"expected_version": c.Version, "mode": "PRE_PROVISIONED"}, 200)
	v := version(t, w)
	exp := registrytest.T0.Add(900 * time.Hour).Format(time.RFC3339)
	w = h.cmd(t, c.ConnectionID, "bind", hTenantA, "creator", map[string]any{"expected_version": v, "bind": map[string]any{
		"auth_profile": "oauth", "credential_ref": hRefOAuth, "granted_scopes": []string{"payments.write", "payments.read"}, "consent_actor": "x", "expires_at": exp}}, 200)
	v = version(t, w)
	w = h.cmd(t, c.ConnectionID, "bind", hTenantA, "creator", map[string]any{"expected_version": v, "bind": map[string]any{
		"auth_profile": "hmac-in", "credential_ref": hRefHMAC, "consent_actor": "x"}}, 200)
	v = version(t, w)
	w = h.cmd(t, c.ConnectionID, "validate", hTenantA, "operator", map[string]any{"expected_version": v}, 200)
	v = version(t, w)
	// activation without probe and certification is refused, with the certification code where it applies
	w = h.cmd(t, c.ConnectionID, "activate", hTenantA, "operator", map[string]any{"expected_version": v}, 409)
	_ = w
	probe := map[string]any{"probe": map[string]any{"account_ref": "acc", "capabilities": []string{"statement-push", "payment-submit", "payment-status"}}}
	if w := h.cmd(t, c.ConnectionID, "probe", hTenantA, "operator", probe, 200); !strings.Contains(w.Body.String(), `"outcome":"PASSED"`) {
		t.Fatalf("%s", w.Body.String())
	}
	cur := h.get(t, hTenantA, c.ConnectionID)
	w = h.cmd(t, c.ConnectionID, "activate", hTenantA, "operator", map[string]any{"expected_version": cur.Version}, 409)
	if p := isProblem(t, w, 409); p["code"] != "XIC-017" {
		t.Fatalf("activation without a certification: %v", p)
	}
	// self-certification is forbidden; a valid certification by someone else lets activation through
	var passed struct {
		Passed []string `json:"passed"`
	}
	_ = json.Unmarshal(registrytest.Passed(), &passed)
	body := map[string]any{"certify": map[string]any{"test_pack_version": "tp-2026.10", "passed": passed.Passed}}
	h.cmd(t, c.ConnectionID, "certify", hTenantA, "creator", body, 403)
	h.cmd(t, c.ConnectionID, "certify", hTenantA, "certifier", body, 200)
	cur = h.get(t, hTenantA, c.ConnectionID)
	h.cmd(t, c.ConnectionID, "activate", hTenantA, "operator", map[string]any{"expected_version": cur.Version}, 200)
	if w := h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/certifications", hTenantA, "x", nil); !strings.Contains(w.Body.String(), `"outcome":"PASSED"`) {
		t.Fatalf("%s", w.Body.String())
	}
}

func TestConnectionHTTP_ValidationAndVersionRules(t *testing.T) {
	h := newConnHarness(t)
	// bad create bodies
	isProblem(t, h.c("POST", "/v1/integration-connections", hTenantA, "creator", []byte(`{`)), 400)
	w := h.c("POST", "/v1/integration-connections", hTenantA, "creator", map[string]any{"environment": "DEV", "connector_id": "bank-x", "contract_version_id": h.v.ContractVersionID, "region": "uk",
		"endpoints": map[string]string{"bank-api": "evil.example"}, "provider_account_ref": "a"})
	p := isProblem(t, w, 422)
	if iss, _ := p["issues"].([]any); len(iss) < 1 {
		t.Fatalf("%v", p)
	}
	c := h.create(t, hTenantA, "SANDBOX", "acc")
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":authorize", hTenantA, "creator", map[string]any{"mode": "PRE_PROVISIONED"}), 400) // no version
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":authorize", hTenantA, "creator", map[string]any{"expected_version": 9, "mode": "PRE_PROVISIONED"}), 409)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":authorize", hTenantA, "creator", map[string]any{"expected_version": c.Version, "mode": "NOPE"}), 422)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":explode", hTenantA, "creator", map[string]any{"expected_version": 1}), 404)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID, hTenantA, "creator", map[string]any{"expected_version": 1}), 404)
	isProblem(t, h.c("POST", "/v1/integration-connections/00000000-0000-4000-8000-000000000000:validate", hTenantA, "creator", map[string]any{"expected_version": 1}), 404)
	isProblem(t, h.c("POST", "/v1/integration-connections/not-a-uuid:validate", hTenantA, "creator", map[string]any{"expected_version": 1}), 404)
	// validation failure: 422, issues, check id
	h.cmd(t, c.ConnectionID, "authorize", hTenantA, "creator", map[string]any{"expected_version": c.Version, "mode": "PRE_PROVISIONED"}, 200)
	cur := h.get(t, hTenantA, c.ConnectionID)
	w = h.cmd(t, c.ConnectionID, "validate", hTenantA, "operator", map[string]any{"expected_version": cur.Version}, 422)
	p = isProblem(t, w, 422)
	if p["check_id"] == nil || !strings.Contains(w.Body.String(), "bindings.oauth") {
		t.Fatalf("%v", p)
	}
	// missing command payloads are 422, not a panic
	cur = h.get(t, hTenantA, c.ConnectionID)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":bind", hTenantA, "creator", map[string]any{"expected_version": cur.Version}), 422)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":rotate", hTenantA, "creator", map[string]any{"expected_version": cur.Version}), 422)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":report-scopes", hTenantA, "creator", map[string]any{"expected_version": cur.Version}), 422)
	// the version in a header works and disagreement is refused
	if w := h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":suspend", hTenantA, "creator", map[string]any{"expected_version": 3, "reason": "x"}, "X-Expected-Version", "4"); w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
}

func TestConnectionHTTP_Idempotency(t *testing.T) {
	h := newConnHarness(t)
	body := map[string]any{"environment": "SANDBOX", "connector_id": "bank-x", "contract_version_id": h.v.ContractVersionID, "region": "uk",
		"endpoints": map[string]string{"bank-api": "api.bank-x.com"}, "provider_account_ref": "acc"}
	if w := h.c("POST", "/v1/integration-connections", hTenantA, "creator", body, "Idempotency-Key", "k1"); w.Code != 201 {
		t.Fatalf("%d", w.Code)
	}
	if w := h.c("POST", "/v1/integration-connections", hTenantA, "creator", body, "Idempotency-Key", "k1"); w.Code != 200 {
		t.Fatalf("replay %d", w.Code)
	}
	body["provider_account_ref"] = "other"
	p := isProblem(t, h.c("POST", "/v1/integration-connections", hTenantA, "creator", body, "Idempotency-Key", "k1"), 409)
	if p["code"] != "INT-IDEM-005" {
		t.Fatalf("%v", p)
	}
	body["provider_account_ref"] = "acc"
	isProblem(t, h.c("POST", "/v1/integration-connections", hTenantA, "creator", body, "Idempotency-Key", "k2"), 409)
}

func TestConnectionHTTP_Authorization(t *testing.T) {
	h := newConnHarness(t)
	body := map[string]any{"environment": "SANDBOX", "connector_id": "bank-x", "contract_version_id": h.v.ContractVersionID, "region": "uk",
		"endpoints": map[string]string{"bank-api": "api.bank-x.com"}, "provider_account_ref": "acc"}
	isProblem(t, h.c("POST", "/v1/integration-connections", hTenantA, "", body), 401)
	if w := h.c("POST", "/v1/integration-connections", "", "creator", body); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	h.authz.deny("intruder", "XIC_CONNECTION_CREATE")
	if p := isProblem(t, h.c("POST", "/v1/integration-connections", hTenantA, "intruder", body), 403); p["code"] != "INT-AUTH-003" {
		t.Fatalf("%v", p)
	}
	if list, _ := h.conns.List(middleware.WithTenant(context.Background(), hTenantA), xicconn.Filter{}); len(list) != 0 {
		t.Fatal("a denied command created a connection")
	}
	// each command asks its own action
	c := h.active(t, hTenantA, "acc")
	got := strings.Join(h.authz.actions, ",")
	for _, a := range []string{"XIC_CONNECTION_CREATE", "XIC_CONNECTION_AUTHORIZE", "XIC_CONNECTION_BIND", "XIC_CONNECTION_VALIDATE", "XIC_CONNECTION_ACTIVATE"} {
		if !strings.Contains(got, a) {
			t.Errorf("%s never asked: %s", a, got)
		}
	}
	h.authz.deny("revoker", "XIC_CONNECTION_REVOKE")
	h.cmd(t, c.ConnectionID, "revoke", hTenantA, "revoker", map[string]any{"expected_version": c.Version, "reason": "x"}, 403)
	if h.get(t, hTenantA, c.ConnectionID).Status != xicconn.StatusActive {
		t.Fatal("a denied revoke took effect")
	}
	h.authz.mu.Lock()
	h.authz.down = true
	h.authz.mu.Unlock()
	w := h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":suspend", hTenantA, "operator", map[string]any{"expected_version": c.Version, "reason": "x"})
	isProblem(t, w, 503)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}

func TestConnectionHTTP_TenantIsolation(t *testing.T) {
	h := newConnHarness(t)
	c := h.active(t, hTenantA, "acc")
	isProblem(t, h.c("GET", "/v1/integration-connections/"+c.ConnectionID, hTenantB, "x", nil), 404)
	for _, p := range []string{"checks", "events", "requests", "certifications"} {
		isProblem(t, h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/"+p, hTenantB, "x", nil), 404)
	}
	if w := h.c("GET", "/v1/integration-connections", hTenantB, "x", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	for _, cmd := range []string{"suspend", "revoke", "validate", "activate", "evaluate"} {
		w := h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":"+cmd, hTenantB, "operator", map[string]any{"expected_version": c.Version, "reason": "x"})
		if w.Code != 404 {
			t.Errorf("%s from another tenant: %d %s", cmd, w.Code, w.Body.String())
		}
	}
	if h.get(t, hTenantA, c.ConnectionID).Status != xicconn.StatusActive {
		t.Fatal("tenant A's connection was disturbed")
	}
}

func TestConnectionHTTP_OAuthStateIsShownOnce(t *testing.T) {
	h := newConnHarness(t)
	// publish an authorization-code contract for a second connector
	h.connector(t, "bank-oauth")
	in := registrytest.Input("1.0.0")
	for i := range in.Content.Profiles {
		if in.Content.Profiles[i].Name == "oauth" {
			in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_AUTH_CODE","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
		}
	}
	w := h.call("POST", "/v1/connectors/bank-oauth/versions", platformTenant, "author", in)
	var v registry.ContractVersion
	decodeInto(t, w, &v)
	_ = v
	// reuse the helper through the service layer for publication of this variant
	pv := h.publishVariant(t, "bank-oauth", in)
	w = h.c("POST", "/v1/integration-connections", hTenantA, "creator", map[string]any{"environment": "SANDBOX", "connector_id": "bank-oauth",
		"contract_version_id": pv.ContractVersionID, "region": "uk", "endpoints": map[string]string{"bank-api": "api.bank-x.com"}, "provider_account_ref": "acc"})
	var c xicconn.Connection
	decodeInto(t, w, &c)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":authorize", hTenantA, "creator",
		map[string]any{"expected_version": c.Version, "mode": "OAUTH_AUTH_CODE", "redirect_uri": "https://evil.example/cb"}), 422)
	w = h.cmd(t, c.ConnectionID, "authorize", hTenantA, "creator", map[string]any{"expected_version": c.Version, "mode": "OAUTH_AUTH_CODE",
		"redirect_uri": "https://app.zoiko.example/oauth/callback"}, 200)
	var res struct {
		State     string `json:"state"`
		SessionID string `json:"session_id"`
	}
	decodeInto(t, w, &res)
	if len(res.State) < 40 || res.SessionID == "" {
		t.Fatalf("%s", w.Body.String())
	}
	// no later read returns the state
	for _, p := range []string{"", "/checks", "/events", "/requests"} {
		if w := h.c("GET", "/v1/integration-connections/"+c.ConnectionID+p, hTenantA, "x", nil); strings.Contains(w.Body.String(), res.State) {
			t.Fatalf("GET %s leaked the authorization state", p)
		}
	}
}

// publishVariant publishes a contract variant through the registry service for the harness.
func (h *connHarness) publishVariant(t *testing.T, connectorID string, in registry.VersionInput) registry.ContractVersion {
	t.Helper()
	// the connector exists; find the draft created by the caller and run it through the lifecycle over HTTP
	vs, _ := h.regHarness.repo.ListVersions(middleware.WithTenant(context.Background(), platformTenant), connectorID)
	v := vs[len(vs)-1]
	full, _ := h.regHarness.repo.GetVersion(middleware.WithTenant(context.Background(), platformTenant), v.ContractVersionID)
	res := h.command(t, full.ContractVersionID, "validate", "author", map[string]any{"expected_version": full.RowVersion}, 200)
	var vr registry.ValidateResult
	decodeInto(t, res, &vr)
	res = h.command(t, full.ContractVersionID, "submit", "author", map[string]any{"expected_version": vr.Version.RowVersion}, 200)
	for _, a := range []struct {
		who, gate string
		detail    any
	}{{"security-reviewer", "SECURITY", nil}, {"privacy-reviewer", "PRIVACY", nil}, {"qa-lead", "NEGATIVE_TESTS", json.RawMessage(registrytest.Passed())},
		{"domain-owner", "DOMAIN_ACCEPTANCE", nil}, {"release-manager", "ROLLBACK", nil}} {
		body := map[string]any{"gate": a.gate, "outcome": "PASSED", "evidence_ref": "REVIEW-" + a.gate}
		if a.detail != nil {
			body["detail"] = a.detail
		}
		h.command(t, full.ContractVersionID, "attest", a.who, body, 200)
	}
	cur, _ := h.regHarness.repo.GetVersion(middleware.WithTenant(context.Background(), platformTenant), full.ContractVersionID)
	res = h.command(t, full.ContractVersionID, "publish", "release-manager", map[string]any{"expected_version": cur.RowVersion}, 200)
	var pub registry.ContractVersion
	decodeInto(t, res, &pub)
	return pub
}

func TestConnectionHTTP_MigrationAndBreakGlassDecisions(t *testing.T) {
	h := newConnHarness(t)
	c := h.active(t, hTenantA, "acc")
	v2 := h.publishedOverHTTP(t, "bank-x", "1.1.0")
	// migration: request (202), self-approval 403, approval by another applies
	w := h.cmd(t, c.ConnectionID, "migrate", hTenantA, "creator", map[string]any{"migration": map[string]any{"to_contract_version_id": v2.ContractVersionID, "reason": "adopt 1.1.0"}}, 202)
	var req xicconn.Request
	decodeInto(t, w, &req)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/"+req.RequestID+":approve", hTenantA, "creator", nil), 403)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/not-there:approve", hTenantA, "approver", nil), 404)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/"+req.RequestID+":explode", hTenantA, "approver", nil), 404)
	w = h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/"+req.RequestID+":approve", hTenantA, "approver", nil)
	if w.Code != 200 || h.get(t, hTenantA, c.ConnectionID).ContractVersionID != v2.ContractVersionID {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/"+req.RequestID+":reject", hTenantA, "approver", nil), 409)

	// break-glass: reveal is refused; a pause is immediate; a resume needs another approver
	cur := h.get(t, hTenantA, c.ConnectionID)
	isProblem(t, h.cmd(t, c.ConnectionID, "break-glass", hTenantA, "operator", map[string]any{"expected_version": cur.Version,
		"break_glass": map[string]any{"action": "REVEAL_CREDENTIAL", "reason": "debug"}}, 403), 403)
	h.cmd(t, c.ConnectionID, "break-glass", hTenantA, "operator", map[string]any{"expected_version": cur.Version,
		"break_glass": map[string]any{"action": "PAUSE", "reason": "suspected leak"}}, 200)
	w = h.cmd(t, c.ConnectionID, "break-glass", hTenantA, "operator", map[string]any{"break_glass": map[string]any{"action": "RESUME", "reason": "contained", "duration_seconds": 600}}, 202)
	var bg xicconn.Request
	decodeInto(t, w, &bg)
	isProblem(t, h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/"+bg.RequestID+":approve", hTenantA, "operator", nil), 403)
	if w := h.c("POST", "/v1/integration-connections/"+c.ConnectionID+"/requests/"+bg.RequestID+":approve", hTenantA, "approver", nil); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if h.get(t, hTenantA, c.ConnectionID).Status != xicconn.StatusValidating {
		t.Fatal("break-glass did not resume to VALIDATING")
	}
	// each kind asks its own authorization action
	got := strings.Join(h.authz.actions, ",")
	for _, a := range []string{"XIC_CONNECTION_MIGRATE_REQUEST", "XIC_CONNECTION_MIGRATE_DECIDE", "XIC_CONNECTION_BREAK_GLASS_REQUEST", "XIC_CONNECTION_BREAK_GLASS_DECIDE"} {
		if !strings.Contains(got, a) {
			t.Errorf("%s never asked", a)
		}
	}
}

func TestConnectionHTTP_RotationScopesAndEvaluate(t *testing.T) {
	h := newConnHarness(t)
	c := h.active(t, hTenantA, "acc")
	exp := registrytest.T0.Add(400 * time.Hour).Format(time.RFC3339)
	w := h.cmd(t, c.ConnectionID, "rotate", hTenantA, "operator", map[string]any{"expected_version": c.Version, "rotate": map[string]any{
		"auth_profile": "oauth", "new_credential_ref": "0a000000-0000-4000-8000-0000000000a9", "grace_seconds": 3600, "expires_at": exp}}, 200)
	if !strings.Contains(w.Body.String(), "previous_grace_until") {
		t.Fatalf("%s", w.Body.String())
	}
	v := version(t, w)
	w = h.cmd(t, c.ConnectionID, "end-grace", hTenantA, "operator", map[string]any{"expected_version": v, "auth_profile": "oauth"}, 200)
	v = version(t, w)
	w = h.cmd(t, c.ConnectionID, "report-scopes", hTenantA, "operator", map[string]any{"expected_version": v, "auth_profile": "oauth", "scopes": []string{"payments.read"}}, 200)
	if !strings.Contains(w.Body.String(), "SCOPE_REDUCED") {
		t.Fatalf("%s", w.Body.String())
	}
	w = h.c("POST", "/v1/integration-connections/"+c.ConnectionID+":evaluate", hTenantA, "operator", map[string]any{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changes"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w = h.c("GET", "/v1/integration-connections/"+c.ConnectionID+"/operability?operation=payment-submit", hTenantA, "x", nil)
	if p := isProblem(t, w, 409); p["code"] != "XIC-001" {
		t.Fatalf("%v", p)
	}
}

func TestConnectionHTTP_EnvelopeIsEnforced(t *testing.T) {
	h := newConnHarness(t)
	body := map[string]any{"environment": "SANDBOX", "connector_id": "bank-x", "contract_version_id": h.v.ContractVersionID, "region": "uk",
		"endpoints": map[string]string{"bank-api": "api.bank-x.com"}, "provider_account_ref": "acc"}
	full := []string{"X-Request-Id", "req-1", "X-Correlation-ID", "corr-1", "X-Source-Channel", "api", "X-Legal-Entity-Id", "11111111-1111-4111-8111-111111111111", "Idempotency-Key", "k-1"}
	for _, drop := range []string{"X-Request-Id", "X-Correlation-ID", "X-Source-Channel", "X-Legal-Entity-Id", "Idempotency-Key"} {
		var hdr []string
		for i := 0; i < len(full); i += 2 {
			if full[i] != drop {
				hdr = append(hdr, full[i], full[i+1])
			}
		}
		if w := h.do(h.cstrict, "POST", "/v1/integration-connections", hTenantA, "creator", body, hdr...); w.Code != 400 {
			t.Errorf("without %s: %d", drop, w.Code)
		}
	}
	if w := h.do(h.cstrict, "POST", "/v1/integration-connections", hTenantA, "creator", body, full...); w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := h.do(h.cstrict, "POST", "/v1/integration-connections/00000000-0000-4000-8000-000000000000:validate", hTenantA, "creator", map[string]any{"expected_version": 1},
		"X-Request-Id", "r", "X-Correlation-ID", "c", "X-Source-Channel", "api", "X-Legal-Entity-Id", "11111111-1111-4111-8111-111111111111"); w.Code != 400 {
		t.Fatalf("a command without an idempotency key was admitted: %d", w.Code)
	}
}
