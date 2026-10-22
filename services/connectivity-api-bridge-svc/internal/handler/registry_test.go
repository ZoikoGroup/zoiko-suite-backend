package handler

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const platformTenant = registry.PlatformScopeID

type regHarness struct {
	r      chi.Router
	strict chi.Router
	repo   *registry.MemRepo
	// fileContract extends the contracts it creates with the CSV file operation.
	fileContract bool
	authz        *fakeAuthz
}

func newRegHarness(t *testing.T) *regHarness {
	t.Helper()
	repo := registry.NewMemRepo()
	az := &fakeAuthz{denied: map[string]bool{}}
	srv := httptest.NewServer(az.handler())
	t.Cleanup(srv.Close)
	now := registrytest.T0
	svc := &registry.Service{Repo: repo, Clock: func() time.Time { now = now.Add(time.Second); return now }}
	h := NewRegistry(svc, repo, authz.NewClient(srv.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRegistryRoutes(r, h)
	strict := chi.NewRouter()
	strict.Use(middleware.TenantContext)
	strict.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeStrict, nil))
	RegisterRegistryRoutes(strict, h)
	return &regHarness{r: r, strict: strict, repo: repo, authz: az}
}

func (h *regHarness) do(router chi.Router, method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
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
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func (h *regHarness) call(method, path, tenant, principal string, body any, hdr ...string) *httptest.ResponseRecorder {
	return h.do(h.r, method, path, tenant, principal, body, hdr...)
}

func decodeInto(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

func (h *regHarness) connector(t *testing.T, id string) {
	t.Helper()
	w := h.call("POST", "/v1/connectors", platformTenant, "author", map[string]any{
		"connector_id": id, "family": "BANK_FEED", "provider_class": "Bank X", "owner": "integration-engineering"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create connector: %d %s", w.Code, w.Body.String())
	}
}

func (h *regHarness) version(t *testing.T, connectorID, semver string) registry.ContractVersion {
	t.Helper()
	in := registrytest.Input(semver)
	if h.fileContract {
		in.Content = registrytest.WithFileOperation(in.Content)
	}
	w := h.call("POST", "/v1/connectors/"+connectorID+"/versions", platformTenant, "author", in)
	if w.Code != http.StatusCreated {
		t.Fatalf("create version: %d %s", w.Code, w.Body.String())
	}
	var v registry.ContractVersion
	decodeInto(t, w, &v)
	return v
}

func (h *regHarness) command(t *testing.T, id, cmd, principal string, body map[string]any, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := h.call("POST", "/v1/contract-versions/"+id+":"+cmd, platformTenant, principal, body)
	if w.Code != want {
		t.Fatalf("%s as %s: %d (want %d) %s", cmd, principal, w.Code, want, w.Body.String())
	}
	return w
}

// publishedOverHTTP takes a version through every step through the API.
func (h *regHarness) publishedOverHTTP(t *testing.T, connectorID, semver string) registry.ContractVersion {
	t.Helper()
	v := h.version(t, connectorID, semver)
	w := h.command(t, v.ContractVersionID, "validate", "author", map[string]any{"expected_version": v.RowVersion}, 200)
	var res registry.ValidateResult
	decodeInto(t, w, &res)
	w = h.command(t, v.ContractVersionID, "submit", "author", map[string]any{"expected_version": res.Version.RowVersion}, 200)
	var cur registry.ContractVersion
	decodeInto(t, w, &cur)
	for _, a := range []struct {
		who, gate string
		detail    any
	}{{"security-reviewer", "SECURITY", nil}, {"privacy-reviewer", "PRIVACY", nil}, {"qa-lead", "NEGATIVE_TESTS", json.RawMessage(registrytest.PassedFor(v.Content))},
		{"domain-owner", "DOMAIN_ACCEPTANCE", nil}, {"release-manager", "ROLLBACK", nil}} {
		body := map[string]any{"gate": a.gate, "outcome": "PASSED", "evidence_ref": "REVIEW-" + a.gate}
		if a.detail != nil {
			body["detail"] = a.detail
		}
		h.command(t, v.ContractVersionID, "attest", a.who, body, 200)
	}
	w = h.call("GET", "/v1/contract-versions/"+v.ContractVersionID, platformTenant, "x", nil)
	decodeInto(t, w, &cur)
	w = h.command(t, v.ContractVersionID, "publish", "release-manager", map[string]any{"expected_version": cur.RowVersion}, 200)
	var pub registry.ContractVersion
	decodeInto(t, w, &pub)
	return pub
}

func isProblem(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d (want %d): %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q", ct)
	}
	var m map[string]any
	decodeInto(t, w, &m)
	for _, k := range []string{"type", "title", "status", "detail"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("problem lacks %s: %v", k, m)
		}
	}
	return m
}

// ---- the lifecycle over HTTP --------------------------------------------------------------

func TestRegistryHTTP_FullLifecycle(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	pub := h.publishedOverHTTP(t, "bank-x", "1.0.0")
	if pub.Status != registry.VersionPublished || pub.PublishedBy != "release-manager" {
		t.Fatalf("%+v", pub)
	}
	// reads
	w := h.call("GET", "/v1/connectors/bank-x", platformTenant, "x", nil)
	var c registry.Connector
	decodeInto(t, w, &c)
	if w.Code != 200 || c.Lifecycle != registry.ConnectorPublished {
		t.Fatalf("%d %+v", w.Code, c)
	}
	w = h.call("GET", "/v1/connectors", platformTenant, "x", nil)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("%s", w.Body.String())
	}
	w = h.call("GET", "/v1/connectors?family=HRIS", platformTenant, "x", nil)
	if !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	w = h.call("GET", "/v1/connectors/bank-x/versions", platformTenant, "x", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) || strings.Contains(w.Body.String(), `"operations"`) {
		t.Fatalf("version list must be a summary: %s", w.Body.String())
	}
	for _, p := range []string{"gates", "transitions", "validation-runs"} {
		w = h.call("GET", "/v1/contract-versions/"+pub.ContractVersionID+"/"+p, platformTenant, "x", nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	w = h.call("GET", "/v1/connectors/bank-x/compatibility", platformTenant, "x", nil)
	var cp registry.Compatibility
	decodeInto(t, w, &cp)
	if w.Code != 200 || cp.Current != pub.ContractVersionID || len(cp.Versions) != 1 || !cp.Versions[0].Supported {
		t.Fatalf("%d %+v", w.Code, cp)
	}
	// every command asked the matching action, and each gate its own
	want := []string{"XIC_CONNECTOR_CREATE", "XIC_CONTRACT_CREATE", "XIC_CONTRACT_VALIDATE", "XIC_CONTRACT_SUBMIT", "XIC_GATE_SECURITY",
		"XIC_GATE_PRIVACY", "XIC_GATE_NEGATIVE_TESTS", "XIC_GATE_DOMAIN_ACCEPTANCE", "XIC_GATE_ROLLBACK", "XIC_CONTRACT_PUBLISH"}
	got := strings.Join(h.authz.actions, ",")
	for _, a := range want {
		if !strings.Contains(got, a) {
			t.Errorf("action %s was never asked of authorization-svc; asked: %s", a, got)
		}
	}
}

func TestRegistryHTTP_FailedValidationIs422WithIssuesAndRunID(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	in := registrytest.Input("1.0.0")
	in.Content.Operations[1].MaxPayloadBytes = 0
	w := h.call("POST", "/v1/connectors/bank-x/versions", platformTenant, "author", in)
	var v registry.ContractVersion
	decodeInto(t, w, &v)
	w = h.command(t, v.ContractVersionID, "validate", "author", map[string]any{"expected_version": v.RowVersion}, 422)
	p := isProblem(t, w, 422)
	issues, ok := p["issues"].([]any)
	if !ok || len(issues) == 0 || p["run_id"] == "" || p["code"] != "INT-SCHEMA-004" {
		t.Fatalf("%v", p)
	}
	// the draft is untouched and the run is readable
	w = h.call("GET", "/v1/contract-versions/"+v.ContractVersionID+"/validation-runs", platformTenant, "x", nil)
	if !strings.Contains(w.Body.String(), `"outcome":"FAILED"`) {
		t.Fatalf("%s", w.Body.String())
	}
}

func TestRegistryHTTP_ReplaceDraft(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	v := h.version(t, "bank-x", "1.0.0")
	in := registrytest.Input("1.0.0")
	in.Content.Operations[0].RateLimitPerMinute = 99
	body := map[string]any{"expected_version": v.RowVersion}
	raw, _ := json.Marshal(in)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for k, val := range m {
		body[k] = val
	}
	w := h.call("PUT", "/v1/contract-versions/"+v.ContractVersionID, platformTenant, "author", body)
	var out registry.ContractVersion
	decodeInto(t, w, &out)
	if w.Code != 200 || out.Content.Operations[0].RateLimitPerMinute != 99 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// stale version
	w = h.call("PUT", "/v1/contract-versions/"+v.ContractVersionID, platformTenant, "author", body)
	isProblem(t, w, 409)
	// after validation: not editable
	h.command(t, v.ContractVersionID, "validate", "author", map[string]any{"expected_version": out.RowVersion}, 200)
	body["expected_version"] = out.RowVersion + 1
	w = h.call("PUT", "/v1/contract-versions/"+v.ContractVersionID, platformTenant, "author", body)
	isProblem(t, w, 409)
	// missing version is a 400
	delete(body, "expected_version")
	w = h.call("PUT", "/v1/contract-versions/"+v.ContractVersionID, platformTenant, "author", body)
	isProblem(t, w, 400)
}

// ---- routing of {id}:command -----------------------------------------------------------

func TestRegistryHTTP_CommandRouting(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	v := h.version(t, "bank-x", "1.0.0")
	for name, path := range map[string]string{
		"unknown command":         "/v1/contract-versions/" + v.ContractVersionID + ":explode",
		"no command":              "/v1/contract-versions/" + v.ContractVersionID,
		"empty command":           "/v1/contract-versions/" + v.ContractVersionID + ":",
		"mapping unknown cmd":     "/v1/mapping-versions/" + v.MappingVersions[0].MappingVersionID + ":publish",
		"connector unknown cmd":   "/v1/connectors/bank-x:explode",
		"connector no cmd":        "/v1/connectors/bank-x",
		"command on the wrong id": "/v1/contract-versions/00000000-0000-4000-8000-000000000000:validate",
	} {
		w := h.call("POST", path, platformTenant, "author", map[string]any{"expected_version": 1})
		if w.Code != 404 && w.Code != 405 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// a garbled id is a 404, never a 500
	w := h.call("POST", "/v1/contract-versions/not-a-uuid:validate", platformTenant, "author", map[string]any{"expected_version": 1})
	if w.Code != 404 {
		t.Errorf("garbled id: %d", w.Code)
	}
	// ids containing colons route by the LAST colon
	id, cmd := split("a:b:validate")
	if id != "a:b" || cmd != "validate" {
		t.Errorf("split: %q %q", id, cmd)
	}
}

// ---- authorization -----------------------------------------------------------------------

func TestRegistryHTTP_Authorization(t *testing.T) {
	h := newRegHarness(t)
	body := map[string]any{"connector_id": "bank-x", "family": "BANK_FEED", "provider_class": "x", "owner": "y"}
	// no principal: 401
	isProblem(t, h.call("POST", "/v1/connectors", platformTenant, "", body), 401)
	// no tenant: 401 from the tenant middleware
	if w := h.call("POST", "/v1/connectors", "", "author", body); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	// denied: 403 and nothing created
	h.authz.deny("intruder", "XIC_CONNECTOR_CREATE")
	p := isProblem(t, h.call("POST", "/v1/connectors", platformTenant, "intruder", body), 403)
	if p["code"] != "INT-AUTH-003" {
		t.Fatalf("%v", p)
	}
	if list, _ := h.repo.ListConnectors(middleware.WithTenant(testCtx(), platformTenant), "", ""); len(list) != 0 {
		t.Fatal("a denied command created a connector")
	}
	// authorization-svc down: 503 with Retry-After, fail closed
	h.authz.mu.Lock()
	h.authz.down = true
	h.authz.mu.Unlock()
	w := h.call("POST", "/v1/connectors", platformTenant, "someone-new", body)
	isProblem(t, w, 503)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After on an outage")
	}
	if list, _ := h.repo.ListConnectors(middleware.WithTenant(testCtx(), platformTenant), "", ""); len(list) != 0 {
		t.Fatal("an unauthorized-by-outage command created a connector")
	}
}

func TestRegistryHTTP_PerGateAuthorization(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	v := h.version(t, "bank-x", "1.0.0")
	w := h.command(t, v.ContractVersionID, "validate", "author", map[string]any{"expected_version": v.RowVersion}, 200)
	var res registry.ValidateResult
	decodeInto(t, w, &res)
	h.command(t, v.ContractVersionID, "submit", "author", map[string]any{"expected_version": res.Version.RowVersion}, 200)
	// the reviewer who may attest PRIVACY may not attest SECURITY
	h.authz.deny("privacy-reviewer", "XIC_GATE_SECURITY")
	h.command(t, v.ContractVersionID, "attest", "privacy-reviewer", map[string]any{"gate": "SECURITY", "outcome": "PASSED", "evidence_ref": "REVIEW-1"}, 403)
	h.command(t, v.ContractVersionID, "attest", "privacy-reviewer", map[string]any{"gate": "PRIVACY", "outcome": "PASSED", "evidence_ref": "REVIEW-1"}, 200)
	// an unknown gate never reaches authorization as an arbitrary action
	before := len(h.authz.actions)
	h.command(t, v.ContractVersionID, "attest", "privacy-reviewer", map[string]any{"gate": "SCHEMA_COMPAT", "outcome": "PASSED", "evidence_ref": "REVIEW-1"}, 422)
	h.command(t, v.ContractVersionID, "attest", "privacy-reviewer", map[string]any{"gate": "x'; DROP TABLE", "outcome": "PASSED", "evidence_ref": "REVIEW-1"}, 422)
	if len(h.authz.actions) != before {
		t.Fatal("an invalid gate caused an authorization call")
	}
}

func TestRegistryHTTP_SeparationOfDutiesIs403(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	v := h.version(t, "bank-x", "1.0.0")
	w := h.command(t, v.ContractVersionID, "validate", "author", map[string]any{"expected_version": v.RowVersion}, 200)
	var res registry.ValidateResult
	decodeInto(t, w, &res)
	h.command(t, v.ContractVersionID, "submit", "author", map[string]any{"expected_version": res.Version.RowVersion}, 200)
	p := isProblem(t, h.command(t, v.ContractVersionID, "attest", "author",
		map[string]any{"gate": "SECURITY", "outcome": "PASSED", "evidence_ref": "REVIEW-1"}, 403), 403)
	if p["code"] != "INT-AUTH-003" {
		t.Fatalf("%v", p)
	}
}

func TestRegistryHTTP_TenantCannotWrite(t *testing.T) {
	h := newRegHarness(t)
	// authorization says yes, but the registry is platform data: the scope itself is the gate
	w := h.call("POST", "/v1/connectors", "tenant-a", "tenant-admin", map[string]any{
		"connector_id": "evil-conn", "family": "API", "provider_class": "x", "owner": "y"})
	p := isProblem(t, w, 403)
	if p["code"] != "INT-AUTH-003" {
		t.Fatalf("%v", p)
	}
	h.connector(t, "bank-x")
	v := h.version(t, "bank-x", "1.0.0")
	for _, c := range []struct{ cmd string }{{"validate"}, {"submit"}, {"publish"}, {"block"}} {
		w := h.call("POST", "/v1/contract-versions/"+v.ContractVersionID+":"+c.cmd, "tenant-a", "tenant-admin", map[string]any{"expected_version": 1, "reason": "x"})
		if w.Code != 403 {
			t.Errorf("%s from a tenant: %d %s", c.cmd, w.Code, w.Body.String())
		}
	}
}

// ---- tenant view ------------------------------------------------------------------------

func TestRegistryHTTP_TenantsReadOnlyPublishedAndWithoutStaffIdentities(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	draft := h.version(t, "bank-x", "1.0.0")
	// unpublished: invisible
	isProblem(t, h.call("GET", "/v1/connectors/bank-x", "tenant-a", "t", nil), 404)
	isProblem(t, h.call("GET", "/v1/contract-versions/"+draft.ContractVersionID, "tenant-a", "t", nil), 404)
	isProblem(t, h.call("GET", "/v1/mapping-versions/"+draft.MappingVersions[0].MappingVersionID, "tenant-a", "t", nil), 404)
	if w := h.call("GET", "/v1/connectors", "tenant-a", "t", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("%s", w.Body.String())
	}
	pub := h.publishedOverHTTP(t, "bank-x", "1.0.0-unused"[:0]+"1.1.0")
	_ = draft
	w := h.call("GET", "/v1/contract-versions/"+pub.ContractVersionID, "tenant-a", "t", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var seen registry.ContractVersion
	decodeInto(t, w, &seen)
	if seen.CreatedBy != "" || seen.PublishedBy != "" || seen.RollbackRef != "" || seen.BlockedReason != "" {
		t.Fatalf("a tenant read staff identities or internal references: %+v", seen)
	}
	if seen.ContentHash != pub.ContentHash || len(seen.Content.Operations) != 3 {
		t.Fatal("tenant lost the contract itself")
	}
	w = h.call("GET", "/v1/connectors/bank-x", "tenant-a", "t", nil)
	var c registry.Connector
	decodeInto(t, w, &c)
	if w.Code != 200 || c.CreatedBy != "" || c.Owner == "" {
		t.Fatalf("%d %+v", w.Code, c)
	}
	// evidence names the people behind each decision: platform only, and invisible (404) to a tenant
	for _, p := range []string{"gates", "transitions", "validation-runs"} {
		isProblem(t, h.call("GET", "/v1/contract-versions/"+pub.ContractVersionID+"/"+p, "tenant-a", "t", nil), 404)
	}
	// the compatibility view is open to tenants, redacted
	w = h.call("GET", "/v1/connectors/bank-x/compatibility", "tenant-a", "t", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "author") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// ---- idempotency, versions, validation of input -------------------------------------------------

func TestRegistryHTTP_Idempotency(t *testing.T) {
	h := newRegHarness(t)
	body := map[string]any{"connector_id": "bank-x", "family": "BANK_FEED", "provider_class": "Bank X", "owner": "eng"}
	if w := h.call("POST", "/v1/connectors", platformTenant, "author", body, "Idempotency-Key", "k1"); w.Code != 201 {
		t.Fatalf("%d", w.Code)
	}
	if w := h.call("POST", "/v1/connectors", platformTenant, "author", body, "Idempotency-Key", "k1"); w.Code != 200 {
		t.Fatalf("replay: %d", w.Code)
	}
	other := map[string]any{"connector_id": "bank-x", "family": "BANK_FEED", "provider_class": "Bank X", "owner": "someone"}
	p := isProblem(t, h.call("POST", "/v1/connectors", platformTenant, "author", other, "Idempotency-Key", "k1"), 409)
	if p["code"] != "INT-IDEM-005" {
		t.Fatalf("%v", p)
	}
	// the same id under another key is a plain conflict
	isProblem(t, h.call("POST", "/v1/connectors", platformTenant, "author", body, "Idempotency-Key", "k2"), 409)
}

func TestRegistryHTTP_BadInput(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	// malformed JSON
	isProblem(t, h.call("POST", "/v1/connectors", platformTenant, "author", []byte(`{"connector_id":`)), 400)
	isProblem(t, h.call("POST", "/v1/connectors/bank-x/versions", platformTenant, "author", []byte(`not json`)), 400)
	// invalid connector fields are a 422 with the issues, not a 500
	w := h.call("POST", "/v1/connectors", platformTenant, "author", map[string]any{"connector_id": "X", "family": "MAGIC"})
	p := isProblem(t, w, 422)
	if iss, _ := p["issues"].([]any); len(iss) < 3 {
		t.Fatalf("%v", p)
	}
	// a version for a connector that is not there
	isProblem(t, h.call("POST", "/v1/connectors/ghost/versions", platformTenant, "author", registrytest.Input("1.0.0")), 404)
	// bad list filters
	isProblem(t, h.call("GET", "/v1/connectors?family=MAGIC", platformTenant, "x", nil), 422)
	isProblem(t, h.call("GET", "/v1/connectors?lifecycle=NOPE", platformTenant, "x", nil), 422)
	// an oversized body is refused before it is parsed
	huge := bytes.Repeat([]byte("a"), 6<<20)
	isProblem(t, h.call("POST", "/v1/connectors/bank-x/versions", platformTenant, "author", append([]byte(`{"semver":"`), huge...)), 400)
	// stale / missing versions on commands
	v := h.version(t, "bank-x", "1.0.0")
	isProblem(t, h.call("POST", "/v1/contract-versions/"+v.ContractVersionID+":validate", platformTenant, "author", map[string]any{}), 400)
	isProblem(t, h.call("POST", "/v1/contract-versions/"+v.ContractVersionID+":validate", platformTenant, "author", map[string]any{"expected_version": 99}), 409)
	// the header form of the version works, and disagreement with the body is refused
	if w := h.call("POST", "/v1/contract-versions/"+v.ContractVersionID+":validate", platformTenant, "author", map[string]any{"expected_version": 5}, "X-Expected-Version", "1"); w.Code != 400 {
		t.Fatalf("disagreeing versions: %d", w.Code)
	}
	if w := h.call("POST", "/v1/contract-versions/"+v.ContractVersionID+":validate", platformTenant, "author", nil, "X-Expected-Version", "1"); w.Code != 200 {
		t.Fatalf("header version: %d %s", w.Code, w.Body.String())
	}
}

func TestRegistryHTTP_ProblemsNeverEchoContractValues(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	in := registrytest.Input("1.0.0")
	in.Content.Profiles[0].Spec = json.RawMessage(`{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"","scopes_required":[],"credential_class":"sk_live_SUPERSECRET"}`) // gitleaks:allow (fake value: proves a credential-shaped credential_class is refused)
	w := h.call("POST", "/v1/connectors/bank-x/versions", platformTenant, "author", in)
	var v registry.ContractVersion
	decodeInto(t, w, &v)
	w = h.command(t, v.ContractVersionID, "validate", "author", map[string]any{"expected_version": v.RowVersion}, 422)
	if strings.Contains(w.Body.String(), "sk_live_SUPERSECRET") { // gitleaks:allow
		t.Fatalf("a contract value leaked into a problem: %s", w.Body.String())
	}
}

func TestRegistryHTTP_DeprecateConnector(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	h.publishedOverHTTP(t, "bank-x", "1.0.0")
	w := h.call("GET", "/v1/connectors/bank-x", platformTenant, "x", nil)
	var c registry.Connector
	decodeInto(t, w, &c)
	until := registrytest.T0.Add(90 * 24 * time.Hour).Format(time.RFC3339)
	isProblem(t, h.call("POST", "/v1/connectors/bank-x:deprecate", platformTenant, "author", map[string]any{"supported_until": until, "reason": "x"}), 400)
	isProblem(t, h.call("POST", "/v1/connectors/bank-x:deprecate", platformTenant, "author", map[string]any{"expected_version": c.Version, "supported_until": "2020-01-01T00:00:00Z", "reason": "x"}), 409)
	w = h.call("POST", "/v1/connectors/bank-x:deprecate", platformTenant, "author", map[string]any{"expected_version": c.Version, "supported_until": until, "reason": "provider sunset"})
	decodeInto(t, w, &c)
	if w.Code != 200 || c.Lifecycle != registry.ConnectorDeprecated {
		t.Fatalf("%d %+v", w.Code, c)
	}
}

// ---- mapping simulation ---------------------------------------------------------------------

func TestRegistryHTTP_Simulate(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	v := h.version(t, "bank-x", "1.0.0")
	mid := v.MappingVersions[0].MappingVersionID
	events := len(h.repo.Events)
	body := map[string]any{"samples": []map[string]any{
		{"source": map[string]any{"account": "A1", "balance": "10.5", "currency": "GBP", "booked_at": "2026-10-20T10:00:00+01:00"}},
		{"source": map[string]any{"account": "A1", "balance": "10.555", "currency": "GBP", "booked_at": "2026-10-20T10:00:00Z"}},
		{"source": map[string]any{"account": "A1", "balance": "1", "currency": "GBP", "booked_at": "2026-10-20T10:00:00Z", "holder": map[string]any{"national_id": "AB123456C"}}},
	}}
	w := h.call("POST", "/v1/mapping-versions/"+mid+":simulate", platformTenant, "author", body)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct {
		Mutated bool `json:"mutated"`
		Results []struct {
			Canonical json.RawMessage `json:"canonical"`
			Issues    []struct{ Class, Code string }
		} `json:"results"`
	}
	decodeInto(t, w, &out)
	if out.Mutated || len(out.Results) != 3 || len(out.Results[0].Issues) != 0 || out.Results[1].Issues[0].Code != "precision_loss" ||
		out.Results[2].Issues[0].Code != "prohibited_field" || out.Results[2].Issues[0].Class != "DATA_POLICY" {
		t.Fatalf("%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "AB123456C") {
		t.Fatal("a prohibited source value was echoed")
	}
	if len(h.repo.Events) != events {
		t.Fatal("a simulation emitted an event")
	}
	// limits and errors
	isProblem(t, h.call("POST", "/v1/mapping-versions/"+mid+":simulate", platformTenant, "author", map[string]any{"samples": []any{}}), 422)
	isProblem(t, h.call("POST", "/v1/mapping-versions/00000000-0000-4000-8000-000000000000:simulate", platformTenant, "author", body), 404)
	isProblem(t, h.call("POST", "/v1/mapping-versions/"+mid+":simulate", "tenant-a", "t", body), 404) // a tenant cannot see a draft mapping
	h.authz.deny("nobody", "XIC_MAPPING_SIMULATE")
	isProblem(t, h.call("POST", "/v1/mapping-versions/"+mid+":simulate", platformTenant, "nobody", body), 403)
	// a published mapping is readable and simulatable by a tenant
	h.publishedOverHTTP(t, "bank-x", "2.0.0")
	vs, _ := h.repo.ListVersions(middleware.WithTenant(testCtx(), platformTenant), "bank-x")
	var pubMapping string
	for _, x := range vs {
		if x.Status == registry.VersionPublished {
			full, _ := h.repo.GetVersion(middleware.WithTenant(testCtx(), platformTenant), x.ContractVersionID)
			pubMapping = full.MappingVersions[0].MappingVersionID
		}
	}
	if w := h.call("GET", "/v1/mapping-versions/"+pubMapping, "tenant-a", "t", nil); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if w := h.call("POST", "/v1/mapping-versions/"+pubMapping+":simulate", "tenant-a", "t", body); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// ---- the service envelope applies to every registry route -------------------------------------------

func TestRegistryHTTP_EnvelopeIsEnforcedOnWrites(t *testing.T) {
	h := newRegHarness(t)
	body := map[string]any{"connector_id": "bank-x", "family": "BANK_FEED", "provider_class": "x", "owner": "y"}
	full := []string{"X-Request-Id", "req-1", "X-Correlation-ID", "corr-1", "X-Source-Channel", "api", "X-Legal-Entity-Id", platformTenant, "Idempotency-Key", "k-1"}
	// each missing mandatory header refuses the write
	for _, drop := range []string{"X-Request-Id", "X-Correlation-ID", "X-Source-Channel", "X-Legal-Entity-Id", "Idempotency-Key"} {
		var hdr []string
		for i := 0; i < len(full); i += 2 {
			if full[i] != drop {
				hdr = append(hdr, full[i], full[i+1])
			}
		}
		w := h.do(h.strict, "POST", "/v1/connectors", platformTenant, "author", body, hdr...)
		if w.Code != http.StatusBadRequest {
			t.Errorf("without %s: %d %s", drop, w.Code, w.Body.String())
		}
	}
	if w := h.do(h.strict, "POST", "/v1/connectors", platformTenant, "author", body, full...); w.Code != 201 {
		t.Fatalf("a complete envelope was refused: %d %s", w.Code, w.Body.String())
	}
	// commands are material writes too
	list, _ := h.repo.ListConnectors(middleware.WithTenant(testCtx(), platformTenant), "", "")
	if len(list) != 1 {
		t.Fatalf("%d connectors", len(list))
	}
	w := h.do(h.strict, "POST", "/v1/contract-versions/00000000-0000-4000-8000-000000000000:validate", platformTenant, "author",
		map[string]any{"expected_version": 1}, "X-Request-Id", "r", "X-Correlation-ID", "c", "X-Source-Channel", "api", "X-Legal-Entity-Id", platformTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a command without an idempotency key was admitted: %d", w.Code)
	}
}

func testCtx() context.Context { return context.Background() }
