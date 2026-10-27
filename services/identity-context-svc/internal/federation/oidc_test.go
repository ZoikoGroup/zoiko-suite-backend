package federation

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newVerifier(t *testing.T, idp *testIdP) (*Verifier, *memRepo, *fakeFetcher) {
	t.Helper()
	repo := newMemRepo()
	fetch := &fakeFetcher{doc: idp.jwks()}
	km := NewKeyManager(repo, fetch, repo, nil)
	return NewVerifier(km), repo, fetch
}

func wantReason(t *testing.T, name string, err error, reason string) {
	t.Helper()
	if !errors.Is(err, ErrTokenRejected) || ReasonOf(err) != reason {
		t.Errorf("%s: want rejection %q, got %v", name, reason, err)
	}
}

func TestVerifyAcceptsAGoodToken(t *testing.T) {
	idp := newTestIdP(t)
	v, _, _ := newVerifier(t, idp)
	for name, tok := range map[string]string{"rsa": idp.mint(t, idp.claims()), "ec": idp.mintEC(t, idp.claims())} {
		got, err := v.Verify(context.Background(), testConnection(), testConfig(), tok, "n-1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Issuer != tIssuer || got.Subject != "user-123" || got.TokenHash == "" || got.MFA {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestVerifyRefusesWhatItMust(t *testing.T) {
	idp := newTestIdP(t)
	v, _, _ := newVerifier(t, idp)
	cfg := testConfig()
	now := time.Now()

	cases := map[string]struct {
		mut    func(jwt.MapClaims)
		reason string
	}{
		"wrong issuer":       {func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" }, "issuer_mismatch"},
		"issuer lookalike":   {func(c jwt.MapClaims) { c["iss"] = tIssuer + "/" }, "issuer_mismatch"},
		"wrong audience":     {func(c jwt.MapClaims) { c["aud"] = "someone-else" }, "audience_mismatch"},
		"no audience":        {func(c jwt.MapClaims) { delete(c, "aud") }, "missing_claim"},
		"expired":            {func(c jwt.MapClaims) { c["exp"] = now.Add(-10 * time.Minute).Unix() }, "expired"},
		"no expiry":          {func(c jwt.MapClaims) { delete(c, "exp") }, "missing_claim"},
		"not yet valid":      {func(c jwt.MapClaims) { c["nbf"] = now.Add(10 * time.Minute).Unix() }, "not_yet_valid"},
		"issued in future":   {func(c jwt.MapClaims) { c["iat"] = now.Add(10 * time.Minute).Unix() }, "issued_in_future"},
		"too old":            {func(c jwt.MapClaims) { c["iat"] = now.Add(-2 * time.Hour).Unix(); c["exp"] = now.Add(time.Hour).Unix() }, "too_old"},
		"no iat":             {func(c jwt.MapClaims) { delete(c, "iat") }, "missing_iat"},
		"multi aud, no azp":  {func(c jwt.MapClaims) { c["aud"] = []string{tAudience, "other"} }, "azp_mismatch"},
		"foreign azp":        {func(c jwt.MapClaims) { c["azp"] = "other-client" }, "azp_mismatch"},
		"wrong nonce":        {func(c jwt.MapClaims) { c["nonce"] = "n-2" }, "nonce_mismatch"},
		"no nonce claim":     {func(c jwt.MapClaims) { delete(c, "nonce") }, "nonce_mismatch"},
		"no subject":         {func(c jwt.MapClaims) { delete(c, "sub") }, "subject_missing"},
		"empty subject":      {func(c jwt.MapClaims) { c["sub"] = "" }, "subject_missing"},
		"subject with space": {func(c jwt.MapClaims) { c["sub"] = "a b" }, "subject_missing"},
		"numeric subject":    {func(c jwt.MapClaims) { c["sub"] = 12345 }, "subject_missing"},
	}
	for name, tc := range cases {
		c := idp.claims()
		tc.mut(c)
		got, err := v.Verify(context.Background(), testConnection(), cfg, idp.mint(t, c), "n-1")
		wantReason(t, name, err, tc.reason)
		if got != nil && got.TokenHash == "" {
			t.Errorf("%s: a post-signature refusal must still carry the evidence", name)
		}
	}

	// a request that supplies no nonce for a connection that requires one
	if _, err := v.Verify(context.Background(), testConnection(), cfg, idp.mint(t, idp.claims()), ""); ReasonOf(err) != "nonce_missing" {
		t.Errorf("missing nonce: %v", err)
	}
	// a connection that does not require one ignores it
	nc := cfg
	nc.NonceOptional = true
	if _, err := v.Verify(context.Background(), testConnection(), nc, idp.mint(t, idp.claims()), ""); err != nil {
		t.Errorf("nonce not required: %v", err)
	}
}

func TestVerifyRefusesForgeriesAndAlgorithmConfusion(t *testing.T) {
	idp := newTestIdP(t)
	v, _, _ := newVerifier(t, idp)
	cfg := testConfig()
	good := idp.mint(t, idp.claims())

	// another party's key under the real key id
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.claims())
	tok.Header["kid"] = idp.kid
	forged, _ := tok.SignedString(other)
	_, err := v.Verify(context.Background(), testConnection(), cfg, forged, "n-1")
	wantReason(t, "forged signature", err, "signature_invalid")

	// a tampered payload under a genuine signature
	parts := strings.Split(good, ".")
	var claims map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &claims)
	claims["sub"] = "admin"
	edited, _ := json.Marshal(claims)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(edited) + "." + parts[2]
	_, err = v.Verify(context.Background(), testConnection(), cfg, tampered, "n-1")
	wantReason(t, "tampered payload", err, "signature_invalid")

	// alg=none
	none := jwt.NewWithClaims(jwt.SigningMethodNone, idp.claims())
	none.Header["kid"] = idp.kid
	noneTok, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	_, err = v.Verify(context.Background(), testConnection(), cfg, noneTok, "n-1")
	wantReason(t, "alg none", err, "unsupported_alg")

	// the classic confusion: HS256 signed with the PUBLIC key bytes as the secret
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, idp.claims())
	hs.Header["kid"] = idp.kid
	hsTok, _ := hs.SignedString([]byte(rsaJWK(idp.kid, &idp.rsaKey.PublicKey)["n"].(string)))
	_, err = v.Verify(context.Background(), testConnection(), cfg, hsTok, "n-1")
	wantReason(t, "alg confusion", err, "unsupported_alg")

	// manually built HS256 using the JWKS document itself as the secret
	mac := hmac.New(sha256.New, idp.jwks())
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT","kid":"rsa-1"}`))
	body, _ := json.Marshal(idp.claims())
	signing := hdr + "." + base64.RawURLEncoding.EncodeToString(body)
	mac.Write([]byte(signing))
	manual := signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	_, err = v.Verify(context.Background(), testConnection(), cfg, manual, "n-1")
	wantReason(t, "manual HS256", err, "unsupported_alg")

	// an algorithm the connection did not approve, even though the key could verify it
	only := cfg
	only.AllowedAlgs = []string{"ES256"}
	_, err = v.Verify(context.Background(), testConnection(), only, good, "n-1")
	wantReason(t, "alg not approved", err, "unsupported_alg")

	// an EC key asked to verify an RSA algorithm (and the reverse) is no key at all
	ecAsRS := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.claims())
	ecAsRS.Header["kid"] = idp.ecKid
	ecAsRSTok, _ := ecAsRS.SignedString(idp.rsaKey)
	_, err = v.Verify(context.Background(), testConnection(), cfg, ecAsRSTok, "n-1")
	wantReason(t, "key type does not fit alg", err, "unknown_key")

	// a key id the IdP does not publish
	unk := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.claims())
	unk.Header["kid"] = "retired-key"
	unkTok, _ := unk.SignedString(idp.rsaKey)
	_, err = v.Verify(context.Background(), testConnection(), cfg, unkTok, "n-1")
	wantReason(t, "unknown kid", err, "unknown_key")

	// garbage and oversized input
	for name, tok := range map[string]string{"empty": "", "garbage": "not.a.jwt", "two parts": "a.b", "huge": strings.Repeat("a", 20000)} {
		if _, err := v.Verify(context.Background(), testConnection(), cfg, tok, "n-1"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := PeekIssuer("garbage"); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("peek garbage: %v", err)
	}
	if iss, err := PeekIssuer(good); err != nil || iss != tIssuer {
		t.Errorf("peek: %q %v", iss, err)
	}
}

func TestWeakAndUnusableKeysAreIgnored(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	idp := newTestIdP(t)
	doc := jwksDoc(
		rsaJWK("weak", &small.PublicKey),
		map[string]any{"kty": "RSA", "kid": "enc", "use": "enc", "n": b64u(idp.rsaKey.N.Bytes()), "e": "AQAB"},
		map[string]any{"kty": "oct", "kid": "sym", "k": "c2VjcmV0"},
		map[string]any{"kty": "EC", "kid": "off-curve", "crv": "P-256", "x": "AQ", "y": "AQ"},
		rsaJWK(idp.kid, &idp.rsaKey.PublicKey),
	)
	if n := usableKeyCount(doc); n != 1 {
		t.Fatalf("only the sound signing key is usable, got %d", n)
	}
	repo := newMemRepo()
	km := NewKeyManager(repo, &fakeFetcher{doc: doc}, repo, nil)
	v := NewVerifier(km)
	weak := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.claims())
	weak.Header["kid"] = "weak"
	tok, _ := weak.SignedString(small)
	if _, err := v.Verify(context.Background(), testConnection(), testConfig(), tok, "n-1"); ReasonOf(err) != "unknown_key" {
		t.Fatalf("a 1024-bit key must not verify anything: %v", err)
	}
}

func TestMFAOnlyUnderTheTenantsApprovedValues(t *testing.T) {
	idp := newTestIdP(t)
	v, _, _ := newVerifier(t, idp)
	c := idp.claims()
	c["amr"] = []string{"pwd", "otp"}
	c["acr"] = "urn:mfa"
	tok := idp.mint(t, c)

	got, _ := v.Verify(context.Background(), testConnection(), testConfig(), tok, "n-1")
	if got == nil || got.MFA {
		t.Fatal("with no approved mfa values an IdP's amr claim means nothing")
	}
	cfg := testConfig()
	cfg.MFAAMRValues = []string{"hwk", "otp"}
	if got, _ := v.Verify(context.Background(), testConnection(), cfg, tok, "n-1"); got == nil || !got.MFA {
		t.Fatal("an approved amr value must count")
	}
	cfg = testConfig()
	cfg.MFAAMRValues = []string{"hwk"}
	if got, _ := v.Verify(context.Background(), testConnection(), cfg, tok, "n-1"); got == nil || got.MFA {
		t.Fatal("an amr value the tenant did not approve must not count")
	}
	cfg = testConfig()
	cfg.MFAACRValues = []string{"urn:mfa"}
	if got, _ := v.Verify(context.Background(), testConnection(), cfg, tok, "n-1"); got == nil || !got.MFA {
		t.Fatal("an approved acr value must count")
	}
}

func TestAcceptsWithinClockSkewOnly(t *testing.T) {
	idp := newTestIdP(t)
	v, _, _ := newVerifier(t, idp)
	now := time.Now()
	c := idp.claims()
	c["exp"] = now.Add(-30 * time.Second).Unix() // inside the 60s skew
	c["iat"] = now.Add(-40 * time.Second).Unix()
	if _, err := v.Verify(context.Background(), testConnection(), testConfig(), idp.mint(t, c), "n-1"); err != nil {
		t.Fatalf("within skew: %v", err)
	}
	c["exp"] = now.Add(-90 * time.Second).Unix()
	_, err := v.Verify(context.Background(), testConnection(), testConfig(), idp.mint(t, c), "n-1")
	wantReason(t, "beyond skew", err, "expired")
}
