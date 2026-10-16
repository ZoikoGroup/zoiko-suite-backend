package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "identity-context-svc"
	testAudience = "zoiko-internal"
	testKid      = "kid-1"
	tenantA      = "11111111-1111-1111-1111-111111111111"
	tenantB      = "22222222-2222-2222-2222-222222222222"
)

type env struct {
	key     *rsa.PrivateKey
	fetches *atomic.Int32
	gate    func(Mode, ...string) *Gate
}

func newEnv(t *testing.T) *env {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": testKid,
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return &env{key: key, fetches: &fetches, gate: func(m Mode, delegates ...string) *Gate {
		return NewGate(m, NewJWKS(srv.URL, time.Minute, nil), testIssuer, testAudience, delegates, nil)
	}}
}

func (e *env) envelope(t *testing.T, mutate func(jwt.MapClaims), kid string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": testIssuer, "aud": testAudience,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		"tenant_id": tenantA,
		"principal": map[string]any{"principal_id": "P-ALICE", "tenant_id": tenantA},
		"session_trust_posture": map[string]any{"mfa_verified": true, "session_context_id": "SC-1"},
		"auth_time": time.Now().Add(-2 * time.Minute).Unix(),
		"amr":       []string{"pwd", "otp"},
	}
	if mutate != nil {
		mutate(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(e.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type seen struct {
	called    bool
	assertion *Assertion
	principal string
	tenant    string
	workload  string
}

func serve(g *Gate, req *http.Request) (*httptest.ResponseRecorder, *seen) {
	s := &seen{}
	h := g.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s.called = true
		s.assertion, _ = FromContext(r.Context())
		s.principal, s.tenant = r.Header.Get("X-Principal-Id"), r.Header.Get("X-Tenant-Id")
		s.workload = r.Header.Get("X-Workload-Id")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w, s
}

func adminReq(bearer string, hdr map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func TestGate_VerifiedEnvelopeBecomesTheIdentity(t *testing.T) {
	e := newEnv(t)
	w, s := serve(e.gate(ModeRequire), adminReq(e.envelope(t, nil, testKid), nil))
	if !s.called || w.Code != http.StatusOK {
		t.Fatalf("valid envelope refused: %d %s", w.Code, w.Body)
	}
	a := s.assertion
	if a == nil || a.PrincipalID != "P-ALICE" || a.TenantID != tenantA || !a.MFAVerified || a.Basis != BasisEnvelope ||
		a.SessionContextID != "SC-1" || a.AuthTime.IsZero() || a.AssuranceLevel() != "MFA" {
		t.Fatalf("assertion: %+v", a)
	}
	if s.principal != "P-ALICE" || s.tenant != tenantA {
		t.Fatalf("headers not set from the envelope: %q %q", s.principal, s.tenant)
	}
}

func TestGate_HeaderThatContradictsTheEnvelopeIsRefused(t *testing.T) {
	e := newEnv(t)
	tok := e.envelope(t, nil, testKid)
	if w, s := serve(e.gate(ModeRequire), adminReq(tok, map[string]string{"X-Principal-Id": "P-MALLORY"})); s.called || w.Code != http.StatusForbidden {
		t.Fatalf("forged principal: %d", w.Code)
	}
	if w, s := serve(e.gate(ModeWarn), adminReq(tok, map[string]string{"X-Tenant-Id": tenantB})); s.called || w.Code != http.StatusForbidden {
		t.Fatalf("foreign tenant (warn mode): %d", w.Code)
	}
	if w, s := serve(e.gate(ModeRequire), adminReq(tok, map[string]string{"X-Principal-Id": "P-ALICE", "X-Tenant-Id": tenantA})); !s.called {
		t.Fatalf("matching headers refused: %d", w.Code)
	}
}

func TestGate_InvalidEnvelopesRefusedEvenInWarnMode(t *testing.T) {
	e := newEnv(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": testIssuer, "aud": testAudience, "exp": time.Now().Add(time.Minute).Unix(),
		"tenant_id": tenantA, "principal": map[string]any{"principal_id": "P-ALICE"},
	})
	forged.Header["kid"] = testKid
	forgedTok, _ := forged.SignedString(other)
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": testIssuer, "aud": testAudience, "exp": time.Now().Add(time.Minute).Unix(),
		"tenant_id": tenantA, "principal": map[string]any{"principal_id": "P-ALICE"},
	}).SignedString([]byte("guess"))

	cases := map[string]string{
		"expired":          e.envelope(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, testKid),
		"wrong audience":   e.envelope(t, func(c jwt.MapClaims) { c["aud"] = "identity-context-svc/resolve" }, testKid),
		"wrong issuer":     e.envelope(t, func(c jwt.MapClaims) { c["iss"] = "rogue" }, testKid),
		"no principal":     e.envelope(t, func(c jwt.MapClaims) { c["principal"] = map[string]any{} }, testKid),
		"tenant disagrees": e.envelope(t, func(c jwt.MapClaims) { c["principal"] = map[string]any{"principal_id": "P", "tenant_id": tenantB} }, testKid),
		"unknown kid":      e.envelope(t, nil, "kid-unknown"),
		"other key":        forgedTok,
		"hs256":            hs,
		"garbage":          "not-a-jwt",
	}
	for name, tok := range cases {
		for _, m := range []Mode{ModeRequire, ModeWarn} {
			if w, s := serve(e.gate(m), adminReq(tok, nil)); s.called || w.Code != http.StatusUnauthorized {
				t.Fatalf("%s (%s): want 401, got %d", name, m, w.Code)
			}
		}
	}
}

func TestGate_NoEnvelope(t *testing.T) {
	e := newEnv(t)
	hdr := map[string]string{"X-Principal-Id": "P-ALICE", "X-Tenant-Id": tenantA}

	if w, s := serve(e.gate(ModeRequire), adminReq("", hdr)); s.called || w.Code != http.StatusUnauthorized ||
		w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("require, header only: %d", w.Code)
	}
	w, s := serve(e.gate(ModeWarn), adminReq("", hdr))
	if !s.called || s.assertion != nil || w.Header().Get("X-Identity-Assertion") != "missing" {
		t.Fatalf("warn, header only: called=%v assertion=%v marker=%q", s.called, s.assertion, w.Header().Get("X-Identity-Assertion"))
	}
	if _, s := serve(e.gate(ModeOff), adminReq("", hdr)); !s.called {
		t.Fatal("off mode refused")
	}
}

func withCert(req *http.Request, cn string) *http.Request {
	req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: cn}}}}}
	return req
}

func TestGate_RegisteredDelegateVouchesOnlyOverVerifiedTLS(t *testing.T) {
	e := newEnv(t)
	hdr := map[string]string{"X-Principal-Id": "P-ALICE", "X-Tenant-Id": tenantA}
	g := e.gate(ModeRequire, "access-control-svc")

	_, s := serve(g, withCert(adminReq("", hdr), "access-control-svc"))
	if !s.called || s.assertion == nil || s.assertion.Basis != BasisDelegate || s.assertion.Delegate != "access-control-svc" ||
		s.assertion.PrincipalID != "P-ALICE" || s.workload != "access-control-svc" || s.assertion.AssuranceLevel() != "" {
		t.Fatalf("registered delegate: %+v", s.assertion)
	}
	if w, s := serve(g, withCert(adminReq("", hdr), "payment-run-svc")); s.called || w.Code != http.StatusUnauthorized {
		t.Fatalf("unregistered certificate: %d", w.Code)
	}
	// Claiming to be the delegate in a header is not a certificate.
	hdr["X-Workload-Id"] = "access-control-svc"
	if w, s := serve(g, adminReq("", hdr)); s.called || w.Code != http.StatusUnauthorized {
		t.Fatalf("delegate by header: %d", w.Code)
	}
	// A delegate that forwards the person's envelope is verified like anyone.
	if _, s := serve(g, withCert(adminReq(e.envelope(t, nil, testKid), nil), "access-control-svc")); s.assertion == nil || s.assertion.Basis != BasisEnvelope {
		t.Fatal("forwarded envelope not preferred over delegation")
	}
}

func TestGate_EvaluationAndProbesAreNotGated(t *testing.T) {
	e := newEnv(t)
	g := e.gate(ModeRequire)
	for _, p := range []string{"/v1/authorize", "/internal/authorization/decisions", "/v1/sod/evaluate", "/healthz", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		if _, s := serve(g, req); !s.called {
			t.Fatalf("%s gated", p)
		}
	}
	for _, p := range []string{"/v1/admin/roles", "/admin/v1/break-glass-sessions", "/v1/support/sessions", "/v1/me/capabilities", "/v1/access-decisions"} {
		if !Applies(p) {
			t.Fatalf("%s not gated", p)
		}
	}
}

func TestJWKS_UnknownKidRefetchIsRateLimited(t *testing.T) {
	e := newEnv(t)
	g := e.gate(ModeRequire)
	for i := 0; i < 20; i++ {
		serve(g, adminReq(e.envelope(t, nil, "forged-kid"), nil))
	}
	if n := e.fetches.Load(); n > 2 {
		t.Fatalf("20 forged kids caused %d JWKS fetches", n)
	}
}
