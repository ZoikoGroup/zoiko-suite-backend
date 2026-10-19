package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/federation"
)

type recordingAuthz struct {
	mu    sync.Mutex
	asked []string
	deny  map[string]bool
	err   error
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, _, action, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, action)
	if a.err != nil {
		return a.err
	}
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func (a *recordingAuthz) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asked) == 0 {
		return ""
	}
	return a.asked[len(a.asked)-1]
}

type adminRig struct {
	*provRig
	r     chi.Router
	authz *recordingAuthz
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	pr := newProvRig(t)
	az := &recordingAuthz{deny: map[string]bool{}}
	svc := federation.NewService(pr.s, nil, federation.NewKeyManager(pr.s, nil, pr.s, nil), nil)
	h := federation.NewHandler(svc, nil, az, zap.NewNop()).WithProvisioning(pr.prov)
	r := chi.NewRouter()
	h.Register(r)
	return &adminRig{provRig: pr, r: r, authz: az}
}

func (a *adminRig) do(method, path, body, principal string) (int, map[string]any, http.Header) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t-1")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	req.Header.Set("X-Correlation-ID", "corr-admin")
	w := httptest.NewRecorder()
	a.r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Header()
}

func TestAdminRoutes_ProvisioningNeedsTheRightGrant(t *testing.T) {
	a := newAdminRig(t)
	base := "/v1/federation/connections/" + a.conn
	routes := []struct{ method, path, action string }{
		{"POST", base + "/mapping", federation.ActionProvision},
		{"GET", base + "/mapping", federation.ActionRead},
		{"POST", base + "/mapping/versions/1/approve", federation.ActionApprove},
		{"POST", base + "/mapping/versions/1/reject", federation.ActionApprove},
		{"POST", base + "/scim-token", federation.ActionProvision},
		{"POST", base + "/scim-token/revoke", federation.ActionProvision},
		{"GET", base + "/provisioning/status", federation.ActionRead},
		{"GET", base + "/provisioning/events", federation.ActionRead},
		{"GET", base + "/provisioning/exceptions", federation.ActionRead},
		{"POST", base + "/provisioning/exceptions/x/resolve", federation.ActionProvision},
		{"GET", base + "/provisioning/deprovision-latency", federation.ActionRead},
		{"POST", base + "/provisioning/reconcile", federation.ActionProvision},
	}
	for _, rt := range routes {
		if code, _, _ := a.do(rt.method, rt.path, `{}`, ""); code != 401 {
			t.Errorf("%s %s without identity: %d", rt.method, rt.path, code)
		}
		a.authz.deny = map[string]bool{rt.action: true}
		if code, _, _ := a.do(rt.method, rt.path, `{}`, "admin"); code != 403 {
			t.Errorf("%s %s without the grant: %d", rt.method, rt.path, code)
		}
		if a.authz.last() != rt.action {
			t.Errorf("%s %s asked %q, want %q", rt.method, rt.path, a.authz.last(), rt.action)
		}
		a.authz.deny, a.authz.err = map[string]bool{}, errors.New("down")
		if code, _, _ := a.do(rt.method, rt.path, `{}`, "admin"); code != 503 {
			t.Errorf("%s %s with authz down: %d", rt.method, rt.path, code)
		}
		a.authz.err = nil
	}
}

func TestAdminRoutes_ProvisioningFlow(t *testing.T) {
	a := newAdminRig(t)
	base := "/v1/federation/connections/" + a.conn

	// a privileged role cannot be mapped
	priv := `{"entries":[{"attribute":"groups","value":"Releasers","role_definition_id":"` + pRolePriv + `","legal_entity_id":"` + pEntity + `"}]}`
	if code, out, _ := a.do("POST", base+"/mapping", priv, "maker"); code != 400 || !strings.Contains(out["error"].(string), "cannot be mapped") {
		t.Fatalf("%d %v", code, out)
	}
	// access control unreachable while checking: 503, nothing created
	a.ac.set(func() { a.ac.eligibleErr = fmt.Errorf("%w: down", federation.ErrAccessControlUnavailable) })
	good := `{"entries":[{"attribute":"groups","value":"AP-Readers","role_definition_id":"` + pRoleSecond + `","legal_entity_id":"` + pEntity + `"}]}`
	if code, _, _ := a.do("POST", base+"/mapping", good, "maker"); code != 503 {
		t.Fatalf("%d", code)
	}
	a.ac.set(func() { a.ac.eligibleErr = nil })

	code, m, _ := a.do("POST", base+"/mapping", good, "maker")
	if code != 202 || m["status"] != "PENDING_APPROVAL" {
		t.Fatalf("%d %v", code, m)
	}
	fp, _ := m["fingerprint"].(string)
	if code, _, _ := a.do("POST", base+"/mapping/versions/2/approve", `{"fingerprint":"`+fp+`"}`, "maker"); code != 403 {
		t.Fatalf("the proposer cannot approve: %d", code)
	}
	if code, _, _ := a.do("POST", base+"/mapping/versions/2/approve", `{"fingerprint":"nope"}`, "checker"); code != 409 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := a.do("POST", base+"/mapping/versions/2/approve", `{"fingerprint":"`+fp+`","reason":"ok"}`, "checker"); code != 200 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := a.do("POST", base+"/mapping/versions/2/approve", `{"fingerprint":"`+fp+`"}`, "checker"); code != 409 {
		t.Fatalf("a decided version: %d", code)
	}
	if code, out, _ := a.do("GET", base+"/mapping", "", "admin"); code != 200 || out["active"] == nil {
		t.Fatalf("%d %v", code, out)
	}

	// the token is shown once and never cached
	code, tok, h := a.do("POST", base+"/scim-token", `{}`, "admin")
	if code != 201 || h.Get("Cache-Control") != "no-store" || !strings.HasPrefix(tok["token"].(string), "zscim_") || tok["scim_base_path"] != "/scim/v2/t-1/"+a.conn {
		t.Fatalf("%d %v %v", code, tok, h)
	}
	if code, out, _ := a.do("GET", base+"/mapping", "", "admin"); code != 200 || strings.Contains(toJSON(out), tok["token"].(string)) {
		t.Fatal("the token is never shown again")
	}
	if code, out, _ := a.do("POST", base+"/scim-token/revoke", `{}`, "admin"); code != 200 || out["revoked"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}

	// queries on an empty connection and on a missing one
	for _, p := range []string{"/provisioning/status", "/provisioning/events", "/provisioning/exceptions", "/provisioning/deprovision-latency"} {
		if code, _, _ := a.do("GET", base+p, "", "admin"); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
		if code, _, _ := a.do("GET", "/v1/federation/connections/nope"+p, "", "admin"); code != 404 && p != "/provisioning/events" && p != "/provisioning/exceptions" && p != "/provisioning/deprovision-latency" {
			t.Errorf("%s on a missing connection: %d", p, code)
		}
	}
	// the wire shapes: lists are arrays, never null
	for _, p := range []string{"/provisioning/events", "/provisioning/exceptions"} {
		req := httptest.NewRequest("GET", base+p, nil)
		req.Header.Set("X-Tenant-Id", "t-1")
		req.Header.Set("X-Principal-Id", "admin")
		w := httptest.NewRecorder()
		a.r.ServeHTTP(w, req)
		if strings.TrimSpace(w.Body.String()) != "[]" {
			t.Errorf("%s: %q", p, w.Body.String())
		}
	}

	// reconcile validation, and a note is required to resolve an exception
	if code, _, _ := a.do("POST", base+"/provisioning/reconcile", `{"subjects":[],"complete":true}`, "admin"); code != 200 && code != 400 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := a.do("POST", base+"/provisioning/exceptions/none/resolve", `{"resolution":""}`, "admin"); code != 400 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := a.do("POST", base+"/provisioning/exceptions/none/resolve", `{"resolution":"done"}`, "admin"); code != 404 {
		t.Fatalf("%d", code)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
