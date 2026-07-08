package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
)

const testSecret = "test-signing-secret-32-bytes-min"

var testCfg = &config.Config{
	JWTSigningSecret:      testSecret,
	JWTIssuer:             "identity-context-svc",
	JWTAudienceInternal:   "zoiko-internal",
	EnvelopeJWTTTLSeconds: 300,
}

// idpClaims mirrors the private struct in jwt.go so we can mint test tokens.
type idpClaims struct {
	TenantID string `json:"tenant_id"`
	MFADone  bool   `json:"mfa_done"`
	jwt.RegisteredClaims
}

func mintToken(t *testing.T, claims idpClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(testSecret))
	require.NoError(t, err)
	return signed
}

func validClaims(expOffset time.Duration) idpClaims {
	now := time.Now()
	return idpClaims{
		TenantID: "tenant-abc",
		MFADone:  false,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "auth0|user-123",
			IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(expOffset)),
		},
	}
}

// ── Happy path ───────────────────────────────────────────────────────────────

func TestVerifyBearer_ValidToken_ReturnsClaimsAndNoError(t *testing.T) {
	verifier := auth.NewJWTVerifier(testCfg)
	tok := mintToken(t, validClaims(5*time.Minute))

	got, err := verifier.VerifyBearer(context.Background(), tok)

	require.NoError(t, err)
	assert.Equal(t, "auth0|user-123", got.Subject)
	assert.Equal(t, "tenant-abc", got.TenantID)
	assert.False(t, got.MFADone)
}

// ── Expiration (GetExpirationTime invoked by jwt library) ─────────────────────

func TestVerifyBearer_ExpiredToken_ReturnsError(t *testing.T) {
	verifier := auth.NewJWTVerifier(testCfg)
	// Token expired 10 minutes ago
	tok := mintToken(t, validClaims(-10*time.Minute))

	got, err := verifier.VerifyBearer(context.Background(), tok)

	require.Error(t, err, "expired token must be rejected")
	assert.Nil(t, got)
	// Confirm the library surfaces the expiry error
	assert.ErrorIs(t, err, jwt.ErrTokenExpired)
}

// ── No expiration claim (WithExpirationRequired option) ───────────────────────

func TestVerifyBearer_TokenWithNoExp_ReturnsError(t *testing.T) {
	verifier := auth.NewJWTVerifier(testCfg)
	// Omit ExpiresAt entirely
	claims := idpClaims{
		TenantID: "tenant-abc",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "auth0|user-123",
			IssuedAt: jwt.NewNumericDate(time.Now()),
			// ExpiresAt intentionally absent
		},
	}
	tok := mintToken(t, claims)

	got, err := verifier.VerifyBearer(context.Background(), tok)

	require.Error(t, err, "token with no exp must be rejected by WithExpirationRequired()")
	assert.Nil(t, got)
	assert.ErrorIs(t, err, jwt.ErrTokenRequiredClaimMissing)
}

// ── Wrong signing method (not HMAC) ──────────────────────────────────────────

func TestVerifyBearer_WrongSigningMethod_ReturnsError(t *testing.T) {
	verifier := auth.NewJWTVerifier(testCfg)

	// The library creates an unsigned "none" token which has a different method
	// Use a manually built token with alg:none to force the method check
	raw := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyIn0."

	got, err := verifier.VerifyBearer(context.Background(), raw)

	require.Error(t, err)
	assert.Nil(t, got)
}

// ── Missing sub claim ─────────────────────────────────────────────────────────

func TestVerifyBearer_MissingSubClaim_ReturnsError(t *testing.T) {
	verifier := auth.NewJWTVerifier(testCfg)
	claims := idpClaims{
		TenantID: "tenant-abc",
		RegisteredClaims: jwt.RegisteredClaims{
			// Subject intentionally absent
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
		},
	}
	tok := mintToken(t, claims)

	got, err := verifier.VerifyBearer(context.Background(), tok)

	require.Error(t, err, "token missing sub must be rejected")
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "sub")
}

// ── Missing tenant_id claim ───────────────────────────────────────────────────

func TestVerifyBearer_MissingTenantID_ReturnsError(t *testing.T) {
	verifier := auth.NewJWTVerifier(testCfg)
	claims := idpClaims{
		// TenantID intentionally absent
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "auth0|user-123",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
		},
	}
	tok := mintToken(t, claims)

	got, err := verifier.VerifyBearer(context.Background(), tok)

	require.Error(t, err, "token missing tenant_id must be rejected")
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "tenant_id")
}

// ── Wrong signing secret (tampered token) ─────────────────────────────────────

func TestVerifyBearer_WrongSecret_ReturnsError(t *testing.T) {
	// Mint with a different secret
	otherCfg := &config.Config{JWTSigningSecret: "completely-different-secret-!!!!"}
	tok := mintToken(t, validClaims(5*time.Minute))

	// Verify with the correct config — secrets mismatch → signature invalid
	verifier := auth.NewJWTVerifier(otherCfg)
	got, err := verifier.VerifyBearer(context.Background(), tok)

	require.Error(t, err, "token signed with different secret must be rejected")
	assert.Nil(t, got)
	assert.ErrorIs(t, err, jwt.ErrTokenSignatureInvalid)
}

// ── JWTSigner: round-trip ─────────────────────────────────────────────────────
// Confirms envelopeClaims.Get* methods are exercised by jwt library during Sign,
// and — since Sign now uses RS256 — that the token only verifies against the
// matching RSA public key, never the old HMAC secret.

// newTestSigner generates a throwaway RSA keypair, writes the private key to a
// temp file (mirroring how NewJWTSigner reads a real key off disk in
// production), and returns both the signer and the public key so the test can
// verify independently — the same relationship a real downstream service has
// via the /.well-known/jwks.json endpoint.
func newTestSigner(t *testing.T) (*auth.JWTSigner, *rsa.PublicKey) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	keyPath := filepath.Join(t.TempDir(), "test_signing_key.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	require.NoError(t, os.WriteFile(keyPath, pemBytes, 0o600))

	cfg := &config.Config{
		JWTIssuer:                testCfg.JWTIssuer,
		JWTAudienceInternal:      testCfg.JWTAudienceInternal,
		EnvelopeJWTTTLSeconds:    testCfg.EnvelopeJWTTTLSeconds,
		JWTSigningPrivateKeyPath: keyPath,
		JWTKeyID:                 "test-key-1",
	}

	signer, err := auth.NewJWTSigner(cfg)
	require.NoError(t, err)

	return signer, &privateKey.PublicKey
}

func TestJWTSigner_SignProducesVerifiableToken(t *testing.T) {
	signer, pubKey := newTestSigner(t)

	now := time.Now()
	envelope := &domain.IdentityContextEnvelope{
		JTI: "jti-001",
		ISS: testCfg.JWTIssuer,
		AUD: testCfg.JWTAudienceInternal,
		IAT: now.Unix(),
		EXP: now.Add(5 * time.Minute).Unix(),
		Principal: domain.PrincipalClaims{
			PrincipalID: "principal-xyz",
			TenantID:    "tenant-abc",
		},
		TenantID:      "tenant-abc",
		LegalEntityID: "entity-001",
		SchemaVersion: "1.0",
	}

	signed, err := signer.Sign(envelope)

	require.NoError(t, err)
	require.NotEmpty(t, signed)

	// Parse back using ONLY the public key — proving the asymmetric property:
	// verification never needs, and never sees, the private key.
	parsed, err := jwt.Parse(signed, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		return pubKey, nil
	})
	require.NoError(t, err)
	assert.True(t, parsed.Valid)

	// Confirm GetExpirationTime / GetIssuer / GetAudience were picked up by the lib
	gotIssuer, _ := parsed.Claims.GetIssuer()
	gotAud, _ := parsed.Claims.GetAudience()
	gotExp, _ := parsed.Claims.GetExpirationTime()

	assert.Equal(t, testCfg.JWTIssuer, gotIssuer)
	assert.Contains(t, []string(gotAud), testCfg.JWTAudienceInternal)
	assert.WithinDuration(t, time.Unix(envelope.EXP, 0), gotExp.Time, time.Second)
}

// TestJWTSigner_TamperedToken_FailsVerification proves the actual security
// property: a token altered after signing must fail verification even though
// it's still structurally a valid-looking JWT.
func TestJWTSigner_TamperedToken_FailsVerification(t *testing.T) {
	signer, pubKey := newTestSigner(t)

	envelope := &domain.IdentityContextEnvelope{
		JTI: "jti-002", ISS: testCfg.JWTIssuer, AUD: testCfg.JWTAudienceInternal,
		IAT: time.Now().Unix(), EXP: time.Now().Add(5 * time.Minute).Unix(),
		Principal:     domain.PrincipalClaims{PrincipalID: "principal-xyz", TenantID: "tenant-abc"},
		TenantID:      "tenant-abc",
		LegalEntityID: "entity-001",
		SchemaVersion: "1.0",
	}
	signed, err := signer.Sign(envelope)
	require.NoError(t, err)

	tampered := signed[:len(signed)-2] + "xx" // flip the last two chars of the signature

	_, err = jwt.Parse(tampered, func(tok *jwt.Token) (any, error) {
		return pubKey, nil
	})
	require.Error(t, err, "a tampered token must not verify")
}
