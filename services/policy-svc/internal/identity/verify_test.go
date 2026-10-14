package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

type staticKeys struct{ pub *rsa.PublicKey }

func (k staticKeys) PublicKey(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if kid != "k1" {
		return nil, ErrKeyNotFound
	}
	return k.pub, nil
}

type fixture struct {
	key  *rsa.PrivateKey
	keys KeySource
}

func newFixture(t *testing.T) fixture {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{key: k, keys: staticKeys{pub: &k.PublicKey}}
}

type tokOpt func(*envelopeClaims, *jwt.Token)

func (f fixture) token(t *testing.T, opts ...tokOpt) string {
	c := &envelopeClaims{TenantID: tenantA}
	c.Principal.PrincipalID = "user-7"
	c.Issuer = "identity-context-svc"
	c.Audience = jwt.ClaimStrings{"zoiko-internal"}
	c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	for _, o := range opts {
		o(c, tok)
	}
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type seen struct {
	tenant, principal, basis string
	called                   bool
}

func run(t *testing.T, v *Verifier, mutate func(*http.Request)) (*httptest.ResponseRecorder, *seen) {
	t.Helper()
	s := &seen{}
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called = true
		s.tenant, s.principal, s.basis = r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id"), r.Header.Get(BasisHeader)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", nil)
	mutate(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, s
}

func withToken(tok string, tenant, principal string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
		if tenant != "" {
			r.Header.Set("X-Tenant-Id", tenant)
		}
		if principal != "" {
			r.Header.Set("X-Principal-Id", principal)
		}
	}
}

func TestEnforce_VerifiedEnvelopeAdmitsAndFillsHeaders(t *testing.T) {
	f := newFixture(t)
	v := NewVerifier(Enforce, f.keys, "identity-context-svc", "zoiko-internal", zap.NewNop())

	_, s := run(t, v, withToken(f.token(t), tenantA, "user-7"))
	if !s.called || s.basis != basisEnvelope {
		t.Fatalf("a valid envelope with agreeing headers must be admitted as verified, got %+v", s)
	}
	_, s = run(t, v, withToken(f.token(t), "", ""))
	if !s.called || s.tenant != tenantA || s.principal != "user-7" {
		t.Fatalf("headers missing from the request must be filled from the envelope, got %+v", s)
	}
}

// X5: a header naming another tenant or principal than the signed envelope is
// the forged-identity case, refused outright.
func TestEnforce_HeadersContradictingTheEnvelopeAreRefused(t *testing.T) {
	f := newFixture(t)
	v := NewVerifier(Enforce, f.keys, "identity-context-svc", "zoiko-internal", zap.NewNop())
	for name, mutate := range map[string]func(*http.Request){
		"foreign tenant":    withToken(f.token(t), tenantB, "user-7"),
		"foreign principal": withToken(f.token(t), tenantA, "ceo"),
	} {
		w, s := run(t, v, mutate)
		if w.Code != http.StatusUnauthorized || s.called {
			t.Errorf("%s: expected 401 before the handler, got %d", name, w.Code)
		}
	}
}

func TestEnforce_BadEnvelopesAreRefused(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	v := NewVerifier(Enforce, f.keys, "identity-context-svc", "zoiko-internal", zap.NewNop())
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"tenant_id": tenantA, "principal": map[string]string{"principal_id": "user-7"}})
	hs.Header["kid"] = "k1"
	hsTok, _ := hs.SignedString([]byte("secret"))
	cases := map[string]string{
		"signed by another key": other.token(t),
		"expired":               f.token(t, func(c *envelopeClaims, _ *jwt.Token) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) }),
		"no expiry":             f.token(t, func(c *envelopeClaims, _ *jwt.Token) { c.ExpiresAt = nil }),
		"wrong issuer":          f.token(t, func(c *envelopeClaims, _ *jwt.Token) { c.Issuer = "someone" }),
		"wrong audience":        f.token(t, func(c *envelopeClaims, _ *jwt.Token) { c.Audience = jwt.ClaimStrings{"public"} }),
		"unknown kid":           f.token(t, func(_ *envelopeClaims, tok *jwt.Token) { tok.Header["kid"] = "k9" }),
		"no tenant claim":       f.token(t, func(c *envelopeClaims, _ *jwt.Token) { c.TenantID = "" }),
		"HMAC":                  hsTok,
		"garbage":               "not.a.token",
	}
	for name, tok := range cases {
		w, s := run(t, v, withToken(tok, tenantA, "user-7"))
		if w.Code != http.StatusUnauthorized || s.called {
			t.Errorf("%s: expected 401, got %d", name, w.Code)
		}
	}
}

func TestEnforce_NoProofIsRefused_PeerCertificateIsEnough(t *testing.T) {
	f := newFixture(t)
	v := NewVerifier(Enforce, f.keys, "identity-context-svc", "zoiko-internal", zap.NewNop())
	w, s := run(t, v, func(r *http.Request) { r.Header.Set("X-Tenant-Id", tenantA); r.Header.Set("X-Principal-Id", "user-7") })
	if w.Code != http.StatusUnauthorized || s.called {
		t.Fatalf("bare headers must be refused in enforce mode, got %d", w.Code)
	}
	_, s = run(t, v, func(r *http.Request) {
		r.Header.Set("X-Tenant-Id", tenantA)
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	})
	if !s.called || s.basis != basisPeer {
		t.Fatalf("an mTLS peer must be admitted, got %+v", s)
	}
}

func TestWarnAndOff_Admit(t *testing.T) {
	f := newFixture(t)
	warn := NewVerifier(Warn, f.keys, "identity-context-svc", "zoiko-internal", zap.NewNop())
	_, s := run(t, warn, func(r *http.Request) { r.Header.Set("X-Tenant-Id", tenantA); r.Header.Set(BasisHeader, basisEnvelope) })
	if !s.called || s.basis != basisAsserted {
		t.Fatalf("warn mode admits bare headers as asserted (and never trusts a caller's basis header), got %+v", s)
	}
	_, s = run(t, warn, withToken(f.token(t), tenantB, "user-7"))
	if !s.called || s.basis != basisAsserted {
		t.Fatalf("warn mode admits a mismatch as asserted, got %+v", s)
	}
	_, s = run(t, NewVerifier(Off, nil, "", "", zap.NewNop()), func(*http.Request) {})
	if !s.called {
		t.Fatal("off mode never refuses")
	}
}

func TestHealthIsExempt(t *testing.T) {
	v := NewVerifier(Enforce, staticKeys{}, "i", "a", zap.NewNop())
	called := false
	h := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !called {
		t.Fatal("probes must not need an identity")
	}
}

func TestParseModeFailsClosed(t *testing.T) {
	if ParseMode("enfroce") != Enforce || ParseMode("") != Enforce || ParseMode("WARN") != Warn || ParseMode("off") != Off {
		t.Fatal("an unrecognised mode must enforce")
	}
	if !errors.Is(ErrKeyNotFound, ErrKeyNotFound) {
		t.Fatal()
	}
}
