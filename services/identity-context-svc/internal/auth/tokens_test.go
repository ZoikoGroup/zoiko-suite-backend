package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/keys"
	idenv "zoiko.io/identity-envelope"
)

func rsaSigner(t *testing.T, kid string) keys.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	s, err := keys.NewPEMSigner(kid, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	require.NoError(t, err)
	return s
}

func ecSigner(t *testing.T, kid string) keys.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalECPrivateKey(k)
	require.NoError(t, err)
	s, err := keys.NewPEMSigner(kid, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	require.NoError(t, err)
	return s
}

var testCfg = &config.Config{JWTIssuer: "identity-context-svc", IdPTokenTTLSeconds: 300}

func TestAccessTokenRoundTripCarriesSessionBindingAndAssurance(t *testing.T) {
	ring, err := keys.NewKeyring(rsaSigner(t, "k1"))
	require.NoError(t, err)
	iss, err := auth.NewIdPTokenIssuer(testCfg, ring)
	require.NoError(t, err)
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	tok, err := iss.IssueGrant(domain.TokenGrant{Subject: "sub-1", TenantID: "t1", AuthTime: at,
		AMR: []string{"pwd", "otp"}, MFADone: true, ClientID: "zoiko-console", SID: "sid-1", CnfJKT: "jkt-1", Scope: "openid"})
	require.NoError(t, err)

	hdr := strings.Split(tok, ".")[0]
	require.NotContains(t, hdr, "HS256")
	vc, err := auth.NewJWTVerifier(testCfg, ring).VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	require.Equal(t, "sub-1", vc.Subject)
	require.Equal(t, "sid-1", vc.SID)
	require.Equal(t, "jkt-1", vc.CnfJKT)
	require.Equal(t, "zoiko-console", vc.ClientID)
	require.Equal(t, domain.AssuranceMFA, vc.AssuranceLevel)
	require.True(t, vc.AuthTime.Equal(at))
}

func TestAccessTokenRefusals(t *testing.T) {
	ring, _ := keys.NewKeyring(rsaSigner(t, "k1"))
	other, _ := keys.NewKeyring(rsaSigner(t, "k1"))
	iss, _ := auth.NewIdPTokenIssuer(testCfg, other)
	forged, err := iss.Issue("sub-1", "t1", false)
	require.NoError(t, err)
	v := auth.NewJWTVerifier(testCfg, ring)
	_, err = v.VerifyBearer(context.Background(), forged)
	require.Error(t, err, "a token signed by another key with the same kid")

	// An identity envelope (typ JWT) is not an access token, even signed by us.
	env, err := ring.Sign(map[string]any{"sub": "sub-1", "tenant_id": "t1", "iss": "identity-context-svc",
		"aud": auth.IdPTokenAudience, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}, "JWT")
	require.NoError(t, err)
	_, err = v.VerifyBearer(context.Background(), env)
	require.Error(t, err)

	// The pre-closure HS256 login token no longer verifies.
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "s", "tenant_id": "t1",
		"iss": "identity-context-svc", "aud": auth.IdPTokenAudience, "exp": time.Now().Add(time.Minute).Unix()}).
		SignedString([]byte(strings.Repeat("x", 48)))
	_, err = v.VerifyBearer(context.Background(), hs)
	require.Error(t, err)

	_, err = auth.NewIdPTokenIssuer(&config.Config{IdPTokenTTLSeconds: 0}, ring)
	require.ErrorIs(t, err, auth.ErrTokenTTLInvalid)
}

// AU-05: a rotation publishes both keys, and an envelope signed by the old
// key still verifies after the switch.
func TestRotationKeepsLiveEnvelopesVerifiable(t *testing.T) {
	oldKey, newKey := rsaSigner(t, "2026-10-a"), ecSigner(t, "2026-10-b")
	before, err := keys.NewKeyring(oldKey, newKey)
	require.NoError(t, err)
	envBefore, err := auth.NewJWTSigner(before).Sign(envelope("zoiko-internal"))
	require.NoError(t, err)

	after, err := keys.NewKeyring(newKey, oldKey)
	require.NoError(t, err)
	envAfter, err := auth.NewJWTSigner(after).Sign(envelope("zoiko-internal"))
	require.NoError(t, err)
	require.Contains(t, header(t, envAfter), `"ES256"`)

	rec := httptest.NewRecorder()
	auth.NewJWKSHandler(after).ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	var doc struct{ Keys []idenv.JWK }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	require.Len(t, doc.Keys, 2)

	ks := idenv.StaticKeys{}
	for _, k := range doc.Keys {
		pub, err := k.PublicKey()
		require.NoError(t, err)
		ks[k.Kid] = pub
	}
	v, err := idenv.NewVerifier(idenv.VerifierOptions{Keys: ks, Issuer: "identity-context-svc", AcceptLegacyAudience: true})
	require.NoError(t, err)
	for _, tok := range []string{envBefore, envAfter} {
		_, err := v.Verify(context.Background(), tok)
		require.NoError(t, err)
	}
}

func TestEnvelopeVerifierAudiences(t *testing.T) {
	ring, _ := keys.NewKeyring(rsaSigner(t, "k1"))
	signer := auth.NewJWTSigner(ring)
	ev := auth.NewEnvelopeVerifier(ring, "identity-context-svc", "zoiko-internal", "identity-context-svc")
	ok, _ := signer.Sign(envelope("zoiko-internal"))
	_, err := ev.Verify(ok)
	require.NoError(t, err)
	exchanged, _ := signer.Sign(envelope("policy-svc"))
	_, err = ev.Verify(exchanged)
	require.Error(t, err, "an envelope exchanged for another service is not for these routes")
	got, err := auth.VerifyAnyAudience(ring, "identity-context-svc", exchanged)
	require.NoError(t, err, "introspection answers for any audience this service minted")
	require.Equal(t, "p1", got.Principal.PrincipalID)
}

func envelope(aud string) *domain.IdentityContextEnvelope {
	now := time.Now()
	return &domain.IdentityContextEnvelope{JTI: "j", ISS: "identity-context-svc", AUD: aud, IAT: now.Unix(),
		EXP: now.Add(5 * time.Minute).Unix(), TenantID: "t1",
		Principal:           domain.PrincipalClaims{PrincipalID: "p1", TenantID: "t1"},
		SessionTrustPosture: domain.SessionTrustClaims{SessionContextID: "sc1"}}
}

func header(t *testing.T, tok string) string {
	t.Helper()
	parts := strings.Split(tok, ".")
	b, err := jwt.NewParser().DecodeSegment(parts[0])
	require.NoError(t, err)
	return string(b)
}
