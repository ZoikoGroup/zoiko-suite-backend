package idenv

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testKey, _ = rsa.GenerateKey(rand.Reader, 2048)

const testKid = "k1"

func envelope(t *testing.T, mutate func(m jwt.MapClaims), hdr map[string]any) string {
	t.Helper()
	now := time.Now()
	m := jwt.MapClaims{
		"iss":       "identity-context-svc",
		"aud":       "policy-svc",
		"iat":       now.Unix(),
		"exp":       now.Add(5 * time.Minute).Unix(),
		"jti":       "j1",
		"tenant_id": "t1",
		"principal": map[string]any{"principal_id": "p1", "tenant_id": "t1"},
		"session_trust_posture": map[string]any{
			"session_context_id": "sc1", "mfa_verified": true,
		},
		"auth_time": now.Add(-90 * time.Second).Unix(),
	}
	if mutate != nil {
		mutate(m)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
	tok.Header["kid"] = testKid
	for k, v := range hdr {
		tok.Header[k] = v
	}
	s, err := tok.SignedString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verifier(t *testing.T, legacy bool) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierOptions{Keys: StaticKeys{testKid: &testKey.PublicKey}, Issuer: "identity-context-svc",
		Audience: "policy-svc", AcceptLegacyAudience: legacy})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifierAcceptsOwnAudience(t *testing.T) {
	c, err := verifier(t, false).Verify(context.Background(), envelope(t, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.PrincipalID() != "p1" || c.TenantID != "t1" || c.SessionContextID() != "sc1" {
		t.Fatalf("claims = %+v", c)
	}
}

func TestVerifierAudience(t *testing.T) {
	other := envelope(t, func(m jwt.MapClaims) { m["aud"] = "authorization-svc" }, nil)
	if _, err := verifier(t, true).Verify(context.Background(), other); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("another service's envelope: err = %v, want ErrWrongAudience (IAM A18)", err)
	}
	legacy := envelope(t, func(m jwt.MapClaims) { m["aud"] = LegacyAudience }, nil)
	if _, err := verifier(t, true).Verify(context.Background(), legacy); err != nil {
		t.Fatalf("legacy audience while accepted: %v", err)
	}
	if _, err := verifier(t, false).Verify(context.Background(), legacy); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("legacy audience after cutover: err = %v", err)
	}
}

func TestVerifierRefuses(t *testing.T) {
	cases := map[string]string{
		"expired":           envelope(t, func(m jwt.MapClaims) { m["exp"] = time.Now().Add(-time.Hour).Unix() }, nil),
		"wrong issuer":      envelope(t, func(m jwt.MapClaims) { m["iss"] = "someone-else" }, nil),
		"no principal":      envelope(t, func(m jwt.MapClaims) { delete(m, "principal") }, nil),
		"tenant disagrees":  envelope(t, func(m jwt.MapClaims) { m["principal"] = map[string]any{"principal_id": "p1", "tenant_id": "t2"} }, nil),
		"access token type": envelope(t, nil, map[string]any{"typ": "at+jwt"}),
		"future auth_time":  envelope(t, func(m jwt.MapClaims) { m["auth_time"] = time.Now().Add(time.Hour).Unix() }, nil),
		"unknown kid":       envelope(t, nil, map[string]any{"kid": "nope"}),
	}
	for name, tok := range cases {
		if _, err := verifier(t, true).Verify(context.Background(), tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "identity-context-svc", "aud": "policy-svc",
		"exp": time.Now().Add(time.Minute).Unix(), "tenant_id": "t1", "principal": map[string]any{"principal_id": "p1"}})
	forged.Header["kid"] = testKid
	s, _ := forged.SignedString(other)
	if _, err := verifier(t, true).Verify(context.Background(), s); !errors.Is(err, ErrInvalid) {
		t.Errorf("forged signature: err = %v", err)
	}
}

func TestJWKSRoundTripAndRotation(t *testing.T) {
	second, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	j1, _ := JWKFromPublicKey("k1", &testKey.PublicKey)
	j2, _ := JWKFromPublicKey("k2", &second.PublicKey)
	var fetches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetches, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []JWK{j1, j2}})
	}))
	defer srv.Close()
	ks := NewJWKS(srv.URL, time.Minute, nil)
	for _, kid := range []string{"k1", "k2"} {
		if _, err := ks.PublicKey(context.Background(), kid); err != nil {
			t.Fatalf("%s: %v", kid, err)
		}
	}
	if _, err := ks.PublicKey(context.Background(), "k3"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("unknown kid: %v", err)
	}
	if _, err := ks.PublicKey(context.Background(), "k3"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("unknown kid again: %v", err)
	}
	if n := atomic.LoadInt32(&fetches); n != 1 {
		t.Fatalf("fetches = %d, want 1: an unknown kid within MinRefresh of a fetch must not refetch", n)
	}
}

func TestThumbprintRFC7638Vector(t *testing.T) {
	k := JWK{Kty: "RSA", E: "AQAB",
		N: "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw"}
	got, err := Thumbprint(k)
	if err != nil {
		t.Fatal(err)
	}
	if want := "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"; got != want {
		t.Fatalf("thumbprint = %s, want %s", got, want)
	}
}

func dpopProof(t *testing.T, key *ecdsa.PrivateKey, method, htu, token string, iat time.Time, jti string) string {
	t.Helper()
	jwk, _ := JWKFromPublicKey("", &key.PublicKey)
	jwk.Kid, jwk.Use, jwk.Alg = "", "", ""
	m := jwt.MapClaims{"htm": method, "htu": htu, "iat": iat.Unix(), "jti": jti}
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		m["ath"] = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, m)
	tok.Header["typ"] = "dpop+jwt"
	tok.Header["jwk"] = jwk
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDPoPVerify(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jkt, _ := ThumbprintOf(&key.PublicKey)
	d := NewDPoPVerifier()
	htu := "https://api.zoiko.test/v1/policies"
	now := time.Now()

	p, err := d.Verify(dpopProof(t, key, "POST", htu+"?x=1", "tok", now, "a"), "POST", htu, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if p.JKT != jkt {
		t.Fatalf("jkt = %s, want %s", p.JKT, jkt)
	}
	if _, err := d.Verify(dpopProof(t, key, "POST", htu, "tok", now, "a"), "POST", htu, "tok"); err == nil {
		t.Fatal("replayed jti accepted")
	}
	bad := map[string]string{
		"method":   dpopProof(t, key, "GET", htu, "tok", now, "b"),
		"htu":      dpopProof(t, key, "POST", "https://api.zoiko.test/v1/other", "tok", now, "c"),
		"ath":      dpopProof(t, key, "POST", htu, "other-token", now, "d"),
		"old iat":  dpopProof(t, key, "POST", htu, "tok", now.Add(-10*time.Minute), "e"),
		"future":   dpopProof(t, key, "POST", htu, "tok", now.Add(10*time.Minute), "f"),
		"no proof": "",
	}
	for name, proof := range bad {
		if _, err := d.Verify(proof, "POST", htu, "tok"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type countingObserver struct {
	decisions   []string
	lags        []time.Duration
	unavailable int
}

func (o *countingObserver) Decision(s string)             { o.decisions = append(o.decisions, s) }
func (o *countingObserver) RevocationLag(d time.Duration) { o.lags = append(o.lags, d) }
func (o *countingObserver) RevocationUnavailable()        { o.unavailable++ }

func TestRevocationCachesAndMeasuresLag(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	var calls int32
	active := true
	var revokedAt int64
	src := StatusFunc(func(context.Context, string, *Claims) (SessionStatus, error) {
		atomic.AddInt32(&calls, 1)
		return SessionStatus{Active: active, InvalidatedAt: revokedAt, Reason: "LOGOUT"}, nil
	})
	obs := &countingObserver{}
	rv := NewRevocation(RevocationOptions{Source: src, ActiveTTL: 5 * time.Second, Observer: obs, Now: clock})
	c, _ := verifier(t, true).Verify(context.Background(), envelope(t, nil, nil))

	for i := 0; i < 3; i++ {
		if err := rv.Check(context.Background(), "tok", c); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("introspections = %d, want 1 within the TTL", calls)
	}
	active, revokedAt = false, now.Add(-1*time.Second).Unix()
	now = now.Add(4 * time.Second)
	if err := rv.Check(context.Background(), "tok", c); err != nil {
		t.Fatalf("inside the TTL the cached live answer stands: %v", err)
	}
	now = now.Add(2 * time.Second)
	if err := rv.Check(context.Background(), "tok", c); !errors.Is(err, ErrRevoked) {
		t.Fatalf("after the TTL: err = %v, want ErrRevoked", err)
	}
	if len(obs.lags) != 1 || obs.lags[0] > 8*time.Second {
		t.Fatalf("lags = %v, want one lag no greater than TTL plus revocation offset", obs.lags)
	}
	if err := rv.Check(context.Background(), "tok", c); !errors.Is(err, ErrRevoked) || calls != 2 {
		t.Fatalf("revoked answer must be remembered: err = %v calls = %d", err, calls)
	}
}

func TestRevocationFailModes(t *testing.T) {
	down := StatusFunc(func(context.Context, string, *Claims) (SessionStatus, error) {
		return SessionStatus{}, errors.New("connection refused")
	})
	c, _ := verifier(t, true).Verify(context.Background(), envelope(t, nil, nil))
	closed := NewRevocation(RevocationOptions{Source: down})
	if err := closed.Check(context.Background(), "tok", c); !errors.Is(err, ErrRevocationUnknown) {
		t.Fatalf("fail closed: err = %v", err)
	}
	open := NewRevocation(RevocationOptions{Source: down, Fail: FailOpen})
	if err := open.Check(context.Background(), "tok", c); err != nil {
		t.Fatalf("fail open: err = %v", err)
	}
	pushed := NewRevocation(RevocationOptions{Source: StatusFunc(func(context.Context, string, *Claims) (SessionStatus, error) {
		return SessionStatus{Active: true}, nil
	})})
	pushed.Revoke("sc1", time.Now(), time.Now().Add(time.Minute))
	if err := pushed.Check(context.Background(), "tok", c); !errors.Is(err, ErrRevoked) {
		t.Fatalf("pushed revocation: err = %v", err)
	}
}

func gateFor(t *testing.T, mode Mode, status StatusSource, peers ...string) (*Gate, *countingObserver) {
	t.Helper()
	obs := &countingObserver{}
	g, err := NewGate(GateOptions{Service: "policy-svc", Mode: mode, Verifier: verifier(t, true),
		Revocation: NewRevocation(RevocationOptions{Source: status}), EnforceBinding: true, Peers: peers,
		Observer: obs, PeerName: func(r *http.Request) string { return r.Header.Get("Test-Peer") }})
	if err != nil {
		t.Fatal(err)
	}
	return g, obs
}

var live = StatusFunc(func(context.Context, string, *Claims) (SessionStatus, error) {
	return SessionStatus{Active: true}, nil
})

func serve(g *Gate, req *http.Request) (*httptest.ResponseRecorder, *http.Request) {
	var seen *http.Request
	rec := httptest.NewRecorder()
	RequestID(g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		w.WriteHeader(http.StatusNoContent)
	}))).ServeHTTP(rec, req)
	return rec, seen
}

func TestGateRequireAndStamping(t *testing.T) {
	g, _ := gateFor(t, ModeRequire, live)

	rec, _ := serve(g, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Type") != ContentTypeProblem {
		t.Fatalf("no envelope: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var prob map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &prob)
	if prob["code"] != "identity_assertion_required" || prob["request_id"] == "" || rec.Header().Get(HeaderZoikoRequestID) == "" {
		t.Fatalf("problem = %v", prob)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+envelope(t, nil, nil))
	req.Header.Set(HeaderAuthnAge, "0")
	req.Header.Set(HeaderSupportContextID, "forged")
	rec, seen := serve(g, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid envelope: %d %s", rec.Code, rec.Body.String())
	}
	if seen.Header.Get(HeaderPrincipalID) != "p1" || seen.Header.Get(HeaderTenantID) != "t1" ||
		seen.Header.Get(HeaderSessionContextID) != "sc1" || seen.Header.Get(HeaderIdentityBasis) != string(BasisEnvelope) {
		t.Fatalf("headers not stamped: %v", seen.Header)
	}
	if age := seen.Header.Get(HeaderAuthnAge); age == "0" || age == "" {
		t.Fatalf("authn age %q must come from the envelope's auth_time, not the caller", age)
	}
	if seen.Header.Get(HeaderSupportContextID) != "" {
		t.Fatal("a support context the envelope does not carry must be removed")
	}
	if id, ok := FromContext(seen.Context()); !ok || id.Token == "" {
		t.Fatal("identity not on the context")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+envelope(t, nil, nil))
	req.Header.Set(HeaderPrincipalID, "someone-else")
	if rec, _ := serve(g, req); rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched X-Principal-Id: %d", rec.Code)
	}

	if rec, _ := serve(g, httptest.NewRequest(http.MethodGet, "/healthz", nil)); rec.Code != http.StatusNoContent {
		t.Fatalf("probe gated: %d", rec.Code)
	}
}

func TestGateWarnStillRefusesBadEnvelope(t *testing.T) {
	g, _ := gateFor(t, ModeWarn, live)
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set(HeaderPrincipalID, "p9")
	req.Header.Set(HeaderAuthnAge, "1")
	rec, seen := serve(g, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Identity-Assertion") != "missing" {
		t.Fatalf("warn mode: %d", rec.Code)
	}
	if seen.Header.Get(HeaderAuthnAge) != "" {
		t.Fatal("an unverified caller's authn age must not pass through")
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	if rec, _ := serve(g, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid envelope in warn mode: %d", rec.Code)
	}
}

func TestGateRevokedAndUnavailable(t *testing.T) {
	revoked := StatusFunc(func(context.Context, string, *Claims) (SessionStatus, error) {
		return SessionStatus{Active: false, Reason: "LOGOUT"}, nil
	})
	g, obs := gateFor(t, ModeRequire, revoked)
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+envelope(t, nil, nil))
	rec, _ := serve(g, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "session_revoked") {
		t.Fatalf("revoked session: %d %s", rec.Code, rec.Body.String())
	}
	if obs.decisions[len(obs.decisions)-1] != OutcomeRevoked {
		t.Fatalf("decisions = %v", obs.decisions)
	}

	down := StatusFunc(func(context.Context, string, *Claims) (SessionStatus, error) { return SessionStatus{}, errors.New("down") })
	g, _ = gateFor(t, ModeRequire, down)
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+envelope(t, nil, nil))
	rec, _ = serve(g, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("identity down: %d", rec.Code)
	}
}

func TestGateDPoPBinding(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jkt, _ := ThumbprintOf(&key.PublicKey)
	g, _ := gateFor(t, ModeRequire, live, "access-control-svc")
	tok := envelope(t, func(m jwt.MapClaims) { m["cnf"] = map[string]any{"jkt": jkt} }, nil)

	req := httptest.NewRequest(http.MethodPost, "http://policy-svc:8080/v1/x", nil)
	req.Header.Set("Authorization", "DPoP "+tok)
	if rec, _ := serve(g, req); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "DPoP") {
		t.Fatalf("bound envelope without proof: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://policy-svc:8080/v1/x", nil)
	req.Header.Set("Authorization", "DPoP "+tok)
	req.Header.Set(HeaderDPoP, dpopProof(t, key, "POST", "http://policy-svc:8080/v1/x", tok, time.Now(), "p1"))
	if rec, _ := serve(g, req); rec.Code != http.StatusNoContent {
		t.Fatalf("bound envelope with proof: %d %s", rec.Code, rec.Body.String())
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	req = httptest.NewRequest(http.MethodPost, "http://policy-svc:8080/v1/x", nil)
	req.Header.Set("Authorization", "DPoP "+tok)
	req.Header.Set(HeaderDPoP, dpopProof(t, other, "POST", "http://policy-svc:8080/v1/x", tok, time.Now(), "p2"))
	if rec, _ := serve(g, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("proof by another key: %d", rec.Code)
	}

	// A peer workload forwarding the envelope it already verified.
	req = httptest.NewRequest(http.MethodPost, "http://policy-svc:8080/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Test-Peer", "access-control-svc")
	if rec, _ := serve(g, req); rec.Code != http.StatusNoContent {
		t.Fatalf("forwarded by a peer: %d", rec.Code)
	}

	unbound := envelope(t, nil, nil)
	req = httptest.NewRequest(http.MethodPost, "http://policy-svc:8080/v1/x", nil)
	req.Header.Set("Authorization", "DPoP "+unbound)
	if rec, _ := serve(g, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("DPoP scheme with an unbound envelope: %d", rec.Code)
	}
}

func TestGatePeerBasis(t *testing.T) {
	g, _ := gateFor(t, ModeRequire, live, "access-control-svc")
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Test-Peer", "access-control-svc")
	req.Header.Set(HeaderPrincipalID, "p7")
	rec, seen := serve(g, req)
	if rec.Code != http.StatusNoContent || seen.Header.Get(HeaderIdentityBasis) != string(BasisPeer) ||
		seen.Header.Get(HeaderWorkloadID) != "access-control-svc" {
		t.Fatalf("peer: %d %v", rec.Code, seen)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Test-Peer", "random-svc")
	if rec, _ := serve(g, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unlisted peer: %d", rec.Code)
	}
}

func TestForwardAndExchange(t *testing.T) {
	var gotAud string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotAud = r.PostForm.Get("audience")
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "narrow", "expires_in": 300, "token_type": "N_A"})
	}))
	defer srv.Close()
	c, _ := verifier(t, true).Verify(context.Background(), envelope(t, nil, nil))
	ctx := WithIdentity(context.Background(), &Identity{Basis: BasisEnvelope, PrincipalID: "p1", TenantID: "t1", Claims: c, Token: "wide"})

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	Forward(ctx, req)
	if req.Header.Get("Authorization") != "Bearer wide" || req.Header.Get(HeaderAuthnAge) == "" || req.Header.Get(HeaderPrincipalID) != "p1" {
		t.Fatalf("forward headers = %v", req.Header)
	}
	ex := NewExchanger(srv.URL, "", "", nil)
	req = httptest.NewRequest(http.MethodGet, "http://x/", nil)
	if err := ForwardTo(ctx, req, ex, "authorization-svc"); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer narrow" || gotAud != "authorization-svc" {
		t.Fatalf("exchange: auth=%q aud=%q", req.Header.Get("Authorization"), gotAud)
	}
}

func TestRequestIDAdoptsAndMints(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderXRequestID, "client-req-12345")
	h.ServeHTTP(rec, req)
	if rec.Header().Get(HeaderZoikoRequestID) != "client-req-12345" {
		t.Fatalf("legacy id not adopted: %v", rec.Header())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderZoikoRequestID, "bad id with spaces")
	h.ServeHTTP(rec, req)
	if id := rec.Header().Get(HeaderZoikoRequestID); !strings.HasPrefix(id, "req_") || len(id) != 36 {
		t.Fatalf("minted id = %q", id)
	}
}

var _ crypto.PublicKey = (*rsa.PublicKey)(nil)
