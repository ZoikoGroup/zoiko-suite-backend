package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
)

const idpTestSecret = "local-dev-jwt-signing-secret-key-32-chars-long"

func idpConfig(ttl int) *config.Config {
	return &config.Config{
		JWTSigningSecret:   idpTestSecret,
		JWTIssuer:          "identity-context-svc",
		IdPTokenTTLSeconds: ttl,
	}
}

func TestIdPTokenIssuer_MintsTokenTheVerifierAccepts(t *testing.T) {
	cfg := idpConfig(300)
	issuer, err := auth.NewIdPTokenIssuer(cfg)
	require.NoError(t, err)

	token, err := issuer.Issue("idp|someone", "tenant-1", false)
	require.NoError(t, err)

	claims, err := auth.NewJWTVerifier(cfg).VerifyBearer(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, "idp|someone", claims.Subject)
	require.Equal(t, "tenant-1", claims.TenantID)
	require.False(t, claims.MFADone)
}

// VerifyBearer parses with jwt.WithExpirationRequired(), so a minted token
// must always carry exp. A token without one is refused by the verifier, which
// would make every login fail at the next step.
func TestIdPTokenIssuer_TokenAlwaysExpires(t *testing.T) {
	issuer, err := auth.NewIdPTokenIssuer(idpConfig(300))
	require.NoError(t, err)

	token, err := issuer.Issue("idp|someone", "tenant-1", false)
	require.NoError(t, err)

	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	require.NoError(t, err)
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)

	exp, err := claims.GetExpirationTime()
	require.NoError(t, err)
	require.NotNil(t, exp, "a minted token must carry exp")
	require.WithinDuration(t, time.Now().UTC().Add(300*time.Second), exp.Time, 5*time.Second)

	require.NotEmpty(t, claims["jti"], "each token needs a unique id for the audit trail")
	require.Equal(t, "identity-context-svc", claims["iss"])
}

func TestIdPTokenIssuer_ExpiredTokenIsRejected(t *testing.T) {
	cfg := idpConfig(300)

	// Mint directly with an already-past expiry rather than sleeping, so the
	// test is fast and does not depend on wall-clock timing.
	past := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":       "idp|someone",
		"tenant_id": "tenant-1",
		"iat":       time.Now().Add(-2 * time.Hour).Unix(),
		"exp":       time.Now().Add(-time.Hour).Unix(),
	})
	expired, err := past.SignedString([]byte(idpTestSecret))
	require.NoError(t, err)

	_, err = auth.NewJWTVerifier(cfg).VerifyBearer(context.Background(), expired)
	require.Error(t, err, "an expired bearer token must not resolve")
}

// The token is symmetric, so a different secret must not verify. This is what
// confines minting authority to the process holding JWT_SIGNING_SECRET.
func TestIdPTokenIssuer_ForeignSecretIsRejected(t *testing.T) {
	issuer, err := auth.NewIdPTokenIssuer(idpConfig(300))
	require.NoError(t, err)
	token, err := issuer.Issue("idp|someone", "tenant-1", false)
	require.NoError(t, err)

	other := idpConfig(300)
	other.JWTSigningSecret = strings.Repeat("z", 48)

	_, err = auth.NewJWTVerifier(other).VerifyBearer(context.Background(), token)
	require.Error(t, err)
}

// mfa_done travels into the trust posture calculation, so it must survive the
// round trip honestly rather than being dropped to a default.
func TestIdPTokenIssuer_MFAClaimRoundTrips(t *testing.T) {
	cfg := idpConfig(300)
	issuer, err := auth.NewIdPTokenIssuer(cfg)
	require.NoError(t, err)

	token, err := issuer.Issue("idp|someone", "tenant-1", true)
	require.NoError(t, err)

	claims, err := auth.NewJWTVerifier(cfg).VerifyBearer(context.Background(), token)
	require.NoError(t, err)
	require.True(t, claims.MFADone)
}

func TestIdPTokenIssuer_RejectsBadConstruction(t *testing.T) {
	t.Run("zero TTL", func(t *testing.T) {
		_, err := auth.NewIdPTokenIssuer(idpConfig(0))
		require.ErrorIs(t, err, auth.ErrTokenTTLInvalid)
	})
	t.Run("negative TTL", func(t *testing.T) {
		_, err := auth.NewIdPTokenIssuer(idpConfig(-1))
		require.ErrorIs(t, err, auth.ErrTokenTTLInvalid)
	})
	t.Run("short secret", func(t *testing.T) {
		cfg := idpConfig(300)
		cfg.JWTSigningSecret = "too-short"
		_, err := auth.NewIdPTokenIssuer(cfg)
		require.Error(t, err)
	})
}

func TestIdPTokenIssuer_RejectsEmptyIdentity(t *testing.T) {
	issuer, err := auth.NewIdPTokenIssuer(idpConfig(300))
	require.NoError(t, err)

	_, err = issuer.Issue("", "tenant-1", false)
	require.Error(t, err, "a token with no subject would resolve to no principal")

	_, err = issuer.Issue("idp|someone", "", false)
	require.Error(t, err, "a token with no tenant would resolve against no scope")
}

// Two tokens minted back to back must differ, so a replayed one is
// identifiable in the audit trail.
func TestIdPTokenIssuer_TokensAreUnique(t *testing.T) {
	issuer, err := auth.NewIdPTokenIssuer(idpConfig(300))
	require.NoError(t, err)

	first, err := issuer.Issue("idp|someone", "tenant-1", false)
	require.NoError(t, err)
	second, err := issuer.Issue("idp|someone", "tenant-1", false)
	require.NoError(t, err)

	require.NotEqual(t, first, second)
}
