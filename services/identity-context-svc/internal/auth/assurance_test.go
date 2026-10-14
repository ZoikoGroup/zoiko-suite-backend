package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/domain"
)

// auth_time, amr and azp survive the round trip through the login token.
func TestIdPToken_CarriesAuthTimeAMRAndClient(t *testing.T) {
	cfg := idpConfig(300)
	issuer, err := auth.NewIdPTokenIssuer(cfg)
	require.NoError(t, err)
	at := time.Now().Add(-90 * time.Second).Truncate(time.Second)

	tok, err := issuer.IssueGrant(domain.TokenGrant{
		Subject: "idp|x", TenantID: "t-1", MFADone: true, AuthTime: at,
		AMR: []string{"pwd", "otp"}, ClientID: "zoiko-console",
	})
	require.NoError(t, err)
	claims, err := auth.NewJWTVerifier(cfg).VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	require.True(t, claims.MFADone)
	require.Equal(t, at.UTC(), claims.AuthTime)
	require.Equal(t, []string{"pwd", "otp"}, claims.AMR)
	require.Equal(t, "zoiko-console", claims.ClientID)
}

// A token minted before auth_time existed authenticated at its iat.
func TestIdPToken_LegacyTokenFallsBackToIAT(t *testing.T) {
	cfg := idpConfig(300)
	iat := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "idp|x", "tenant_id": "t-1", "iss": cfg.JWTIssuer,
		"aud": auth.IdPTokenAudience, "iat": iat.Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	}).SignedString([]byte(idpTestSecret))
	require.NoError(t, err)
	claims, err := auth.NewJWTVerifier(cfg).VerifyBearer(context.Background(), tok)
	require.NoError(t, err)
	require.Equal(t, iat.UTC(), claims.AuthTime)
}

// A token claiming to have authenticated in the future is refused.
func TestIdPToken_FutureAuthTimeRefused(t *testing.T) {
	cfg := idpConfig(300)
	issuer, err := auth.NewIdPTokenIssuer(cfg)
	require.NoError(t, err)
	tok, err := issuer.IssueGrant(domain.TokenGrant{Subject: "idp|x", TenantID: "t-1", AuthTime: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	_, err = auth.NewJWTVerifier(cfg).VerifyBearer(context.Background(), tok)
	require.Error(t, err)
}

func signedEnvelope(t *testing.T, mutate func(*domain.IdentityContextEnvelope)) (string, *rsa.PublicKey) {
	t.Helper()
	signer, pub := newTestSigner(t)
	now := time.Now()
	env := &domain.IdentityContextEnvelope{
		ISS: testCfg.JWTIssuer, AUD: testCfg.JWTAudienceInternal,
		IAT: now.Unix(), EXP: now.Add(5 * time.Minute).Unix(),
		Principal: domain.PrincipalClaims{PrincipalID: "p-1", TenantID: "t-1"}, TenantID: "t-1",
		AuthTime: now.Add(-time.Minute).Unix(), AMR: []string{"pwd"}, AuthorizedParty: "zoiko-console",
		SessionTrustPosture: domain.SessionTrustClaims{SessionContextID: "s-1", MFAVerified: true},
	}
	if mutate != nil {
		mutate(env)
	}
	tok, err := signer.Sign(env)
	require.NoError(t, err)
	return tok, pub
}

func TestEnvelopeVerifier_AcceptsItsOwnEnvelope(t *testing.T) {
	tok, pub := signedEnvelope(t, nil)
	env, err := auth.NewEnvelopeVerifier(pub, testCfg.JWTIssuer, testCfg.JWTAudienceInternal).Verify(tok)
	require.NoError(t, err)
	require.Equal(t, "p-1", env.Principal.PrincipalID)
	require.Equal(t, "s-1", env.SessionTrustPosture.SessionContextID)
	require.True(t, env.SessionTrustPosture.MFAVerified)
	require.Equal(t, "zoiko-console", env.AuthorizedParty)
	require.NotZero(t, env.AuthTime)
}

func TestEnvelopeVerifier_Refusals(t *testing.T) {
	cases := map[string]func(*domain.IdentityContextEnvelope){
		"expired":         func(e *domain.IdentityContextEnvelope) { e.EXP = time.Now().Add(-time.Minute).Unix() },
		"wrong audience":  func(e *domain.IdentityContextEnvelope) { e.AUD = "someone-else" },
		"wrong issuer":    func(e *domain.IdentityContextEnvelope) { e.ISS = "rogue" },
		"no principal":    func(e *domain.IdentityContextEnvelope) { e.Principal.PrincipalID = "" },
		"tenant disagree": func(e *domain.IdentityContextEnvelope) { e.Principal.TenantID = "t-2" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			tok, pub := signedEnvelope(t, mut)
			_, err := auth.NewEnvelopeVerifier(pub, testCfg.JWTIssuer, testCfg.JWTAudienceInternal).Verify(tok)
			require.Error(t, err)
		})
	}
	t.Run("another key", func(t *testing.T) {
		tok, _ := signedEnvelope(t, nil)
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		_, err = auth.NewEnvelopeVerifier(&other.PublicKey, testCfg.JWTIssuer, testCfg.JWTAudienceInternal).Verify(tok)
		require.Error(t, err)
	})
	t.Run("hs256 forgery with the public key", func(t *testing.T) {
		_, pub := signedEnvelope(t, nil)
		forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": testCfg.JWTIssuer, "aud": testCfg.JWTAudienceInternal, "exp": time.Now().Add(time.Minute).Unix(),
			"tenant_id": "t-1", "principal": map[string]any{"principal_id": "p-1"},
		}).SignedString([]byte("anything"))
		require.NoError(t, err)
		_, err = auth.NewEnvelopeVerifier(pub, testCfg.JWTIssuer, testCfg.JWTAudienceInternal).Verify(forged)
		require.Error(t, err)
	})
}

func TestClientRegistry(t *testing.T) {
	reg := auth.NewClientRegistry(map[string]string{"zoiko-console": "s3cret-value"}, false)

	id, err := reg.Authenticate("zoiko-console", "s3cret-value")
	require.NoError(t, err)
	require.Equal(t, "zoiko-console", id)

	for name, c := range map[string][2]string{
		"wrong secret":   {"zoiko-console", "nope"},
		"empty secret":   {"zoiko-console", ""},
		"unknown client": {"rogue", "s3cret-value"},
		"secret, no id":  {"", "s3cret-value"},
	} {
		_, err := reg.Authenticate(c[0], c[1])
		require.ErrorIs(t, err, auth.ErrClientInvalid, name)
	}

	id, err = reg.Authenticate("", "")
	require.NoError(t, err)
	require.Empty(t, id, "no client presented and none required")

	_, err = auth.NewClientRegistry(map[string]string{"zoiko-console": "x"}, true).Authenticate("", "")
	require.True(t, errors.Is(err, auth.ErrClientRequired))
}

func TestLoadClientRegistry(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "clients.json")
	// Any well-formed 32-byte hex digest; only the file's shape is under test.
	require.NoError(t, os.WriteFile(good, []byte(`{"zoiko-console":"sha256:e0b6cde1e07b3e9c5d0b24a2c2f55e5ab1d0aa69a7ea0d3cbd6b1c6cdb5bb1d0"}`), 0o600))
	reg, err := auth.LoadClientRegistry(good, false)
	require.NoError(t, err)
	require.Equal(t, 1, reg.Count())

	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"zoiko-console":"plaintext"}`), 0o600))
	_, err = auth.LoadClientRegistry(bad, false)
	require.Error(t, err)

	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, []byte(`{}`), 0o600))
	_, err = auth.LoadClientRegistry(empty, true)
	require.Error(t, err, "required with no clients")
}
