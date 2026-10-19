package store_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/federation"
)

type scimRig struct {
	*provRig
	srv   http.Handler
	token string
	base  string
}

func newSCIMRig(t *testing.T) *scimRig {
	t.Helper()
	pr := newProvRig(t)
	h := federation.NewHandler(nil, nil, nil, zap.NewNop()).WithProvisioning(pr.prov)
	return &scimRig{provRig: pr, srv: h.SCIMRouter(), token: pr.token(t, "t-1", pr.conn), base: "/scim/v2/t-1/" + pr.conn}
}

func (s *scimRig) do(method, path, body, token string, hdr map[string]string) (int, map[string]any, http.Header) {
	req := httptest.NewRequest(method, s.base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.srv.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Header()
}

func TestSCIMHTTP_AuthenticationBeforeAnything(t *testing.T) {
	s := newSCIMRig(t)
	body := userJSON("dir-1", "alice@corp.example", true)
	before := s.count(t, `SELECT count(*) FROM federation_provisioning_events`)

	var bodies []string
	for name, tok := range map[string]string{"no token": "", "wrong token": "zscim_wrong", "a token for another connection": s.token + "x"} {
		code, out, h := s.do("POST", "/Users", body, tok, nil)
		if code != 401 || h.Get("WWW-Authenticate") == "" || !strings.Contains(h.Get("Content-Type"), "application/scim+json") {
			t.Errorf("%s: %d %v", name, code, h)
		}
		raw, _ := json.Marshal(out)
		bodies = append(bodies, string(raw))
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("every unauthenticated refusal is the same answer: %v", bodies)
	}
	if s.count(t, `SELECT count(*) FROM federation_provisioning_events`) != before || s.count(t, `SELECT count(*) FROM principals WHERE identity_provider_subject LIKE 'federated:%'`) != 0 {
		t.Fatal("an unauthenticated caller writes nothing")
	}
	// the token is bound to its own tenant and connection in the path
	req := httptest.NewRequest("GET", "/scim/v2/t-2/"+s.conn+"/ServiceProviderConfig", nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	w := httptest.NewRecorder()
	s.srv.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("another tenant's path: %d", w.Code)
	}
	if code, _, _ := s.do("GET", "/ServiceProviderConfig", "", s.token, nil); code != 200 {
		t.Fatalf("%d", code)
	}
}

func TestSCIMHTTP_UserLifecycle(t *testing.T) {
	s := newSCIMRig(t)
	code, u, h := s.do("POST", "/Users", userJSON("dir-1", "alice@corp.example", true, "AP-Clerks"), s.token, nil)
	if code != 201 {
		t.Fatalf("%d %v", code, u)
	}
	id, _ := u["id"].(string)
	if id == "" || u["externalId"] != "dir-1" || u["active"] != true || h.Get("Location") != s.base+"/Users/"+id {
		t.Fatalf("%v %v", u, h)
	}
	if !strings.Contains(h.Get("Content-Type"), "application/scim+json") || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("%v", h)
	}
	raw, _ := json.Marshal(u)
	if strings.Contains(string(raw), s.token) || strings.Contains(string(raw), "token_hash") {
		t.Fatal("a resource carries no credential")
	}

	// a retry of the same event is answered 200 from the record
	body := userJSON("dir-2", "bob@corp.example", true)
	c1, u1, _ := s.do("POST", "/Users", body, s.token, map[string]string{"X-Event-Id": "evt-77"})
	c2, u2, _ := s.do("POST", "/Users", body, s.token, map[string]string{"X-Event-Id": "evt-77"})
	if c1 != 201 || c2 != 200 || u1["id"] != u2["id"] {
		t.Fatalf("%d %d %v %v", c1, c2, u1, u2)
	}
	// the same id with a different body is refused
	if c, out, _ := s.do("POST", "/Users", userJSON("dir-3", "carol@corp.example", true), s.token, map[string]string{"X-Event-Id": "evt-77"}); c != 409 || out["schemas"] == nil {
		t.Fatalf("%d %v", c, out)
	}
	// Idempotency-Key is honoured too
	i1, ui1, _ := s.do("POST", "/Users", userJSON("dir-4", "dan@corp.example", true), s.token, map[string]string{"Idempotency-Key": "k-1"})
	i2, ui2, _ := s.do("POST", "/Users", userJSON("dir-4", "dan@corp.example", true), s.token, map[string]string{"Idempotency-Key": "k-1"})
	if i1 != 201 || i2 != 200 || ui1["id"] != ui2["id"] {
		t.Fatalf("%d %d", i1, i2)
	}

	if code, got, _ := s.do("GET", "/Users/"+id, "", s.token, nil); code != 200 || got["userName"] != "alice@corp.example" {
		t.Fatalf("%d %v", code, got)
	}
	if code, out, _ := s.do("GET", "/Users/does-not-exist", "", s.token, nil); code != 404 || out["status"] != "404" {
		t.Fatalf("%d %v", code, out)
	}

	// lookups by filter: what a directory does before it creates someone
	for _, f := range []string{`userName eq "alice@corp.example"`, `externalId eq "dir-1"`} {
		code, out, _ := s.do("GET", "/Users?filter="+strings.ReplaceAll(strings.ReplaceAll(f, " ", "%20"), `"`, "%22"), "", s.token, nil)
		if res, _ := out["Resources"].([]any); code != 200 || len(res) != 1 {
			t.Fatalf("%s: %d %v", f, code, out)
		}
	}
	if _, out, _ := s.do("GET", "/Users?filter=externalId%20eq%20%22nobody%22", "", s.token, nil); out["totalResults"] != float64(0) {
		t.Fatalf("%v", out)
	}
	if code, out, _ := s.do("GET", "/Users?filter=active%20eq%20true", "", s.token, nil); code != 400 || out["scimType"] != "invalidFilter" {
		t.Fatalf("%d %v", code, out)
	}
	if _, out, _ := s.do("GET", "/Users", "", s.token, nil); out["totalResults"] != float64(0) {
		t.Fatalf("the whole population is not enumerable: %v", out)
	}

	// replace
	put := `{"userName":"alice@corp.example","externalId":"dir-1","displayName":"Alice Renamed","emails":[{"value":"alice@corp.example"}],"groups":[{"value":"AP-Clerks"}]}`
	if code, got, _ := s.do("PUT", "/Users/"+id, put, s.token, nil); code != 200 || got["displayName"] != "Alice Renamed" {
		t.Fatalf("%d %v", code, got)
	}
	if code, out, _ := s.do("PUT", "/Users/"+id, strings.Replace(put, "dir-1", "dir-OTHER", 1), s.token, nil); code != 400 || out["scimType"] != "mutability" {
		t.Fatalf("the directory's stable id cannot be changed: %d %v", code, out)
	}

	// patch
	patch := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`
	if code, got, _ := s.do("PATCH", "/Users/"+id, patch, s.token, nil); code != 200 || got["active"] != false {
		t.Fatalf("%d %v", code, got)
	}
	if code, out, _ := s.do("PATCH", "/Users/"+id, `{"Operations":[{"op":"add","path":"password","value":"x"}]}`, s.token, nil); code != 400 || out["scimType"] != "invalidPath" {
		t.Fatalf("%d %v", code, out)
	}

	// delete
	if code, _, _ := s.do("DELETE", "/Users/"+id, "", s.token, nil); code != 204 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := s.do("DELETE", "/Users/nope", "", s.token, nil); code != 404 {
		t.Fatalf("%d", code)
	}
}

func TestSCIMHTTP_Refusals(t *testing.T) {
	s := newSCIMRig(t)
	if code, out, _ := s.do("POST", "/Users", `not json`, s.token, nil); code != 400 || out["scimType"] != "invalidSyntax" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out, _ := s.do("POST", "/Users", `{"userName":"a@corp.example"}`, s.token, nil); code != 400 || out["scimType"] != "invalidValue" {
		t.Fatalf("no stable identifier: %d %v", code, out)
	}
	if code, out, _ := s.do("POST", "/Users", userJSON("dir-1", "inactive@corp.example", false), s.token, nil); code != 400 {
		t.Fatalf("a user is not created inactive: %d %v", code, out)
	}
	if code, out, _ := s.do("POST", "/Users", `{"userName":"a@corp.example","externalId":"x","pad":"`+strings.Repeat("a", 2<<20)+`"}`, s.token, nil); code != 413 {
		t.Fatalf("an oversized body: %d %v", code, out)
	}
	// a collision is a SCIM uniqueness error
	s.do("POST", "/Users", userJSON("dir-1", "shared@corp.example", true), s.token, nil)
	if code, out, _ := s.do("POST", "/Users", userJSON("dir-2", "shared@corp.example", true), s.token, nil); code != 409 || out["scimType"] != "uniqueness" {
		t.Fatalf("%d %v", code, out)
	}
	// every error is a SCIM error document
	if _, out, _ := s.do("GET", "/Users/x", "", s.token, nil); out["schemas"] == nil || out["detail"] == nil {
		t.Fatalf("%v", out)
	}
	// a suspended connection grants nothing new
	_, _ = s.s.SetStatus(t.Context(), "t-1", s.conn, federation.ConnSuspended, "op", "stop", "c")
	if code, _, _ := s.do("POST", "/Users", userJSON("dir-5", "late@corp.example", true), s.token, nil); code != 403 {
		t.Fatalf("%d", code)
	}
}
