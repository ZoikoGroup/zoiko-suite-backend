package handler_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/gateway-auth-svc/internal/config"
	"zoiko.io/gateway-auth-svc/internal/handler"
	"zoiko.io/gateway-auth-svc/internal/jwks"
)

const testKid = "test-key-1"

// newTestEnv spins up a throwaway RSA keypair and a fake JWKS server exposing
// its public half, mirroring the real relationship between gateway-auth-svc
// and identity-context-svc's /.well-known/jwks.json endpoint.
func newTestEnv(t *testing.T) (*handler.Handler, *rsa.PrivateKey, *config.Config) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cfg := &config.Config{
		ExpectedIssuer:   "identity-context-svc",
		ExpectedAudience: "zoiko-internal",
		JWKSCacheTTL:     time.Minute,
	}

	jwksSrv := httptest.NewServer(jwksHandler(&privateKey.PublicKey, testKid))
	t.Cleanup(jwksSrv.Close)
	cfg.JWKSURL = jwksSrv.URL

	jwksClient := jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL)
	log := zap.NewNop()

	return handler.New(cfg, jwksClient, log), privateKey, cfg
}

func jwksHandler(pub *rsa.PublicKey, kid string) http.HandlerFunc {
	// Reuse identity-context-svc's own JWKS encoder shape by hand — this
	// service has no import path back to that module, so the wire format is
	// re-derived here from the same RFC 7517 fields.
	return func(w http.ResponseWriter, r *http.Request) {
		eBytes := big.NewInt(int64(pub.E)).Bytes()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","use":"sig","kid":"` + kid + `","alg":"RS256","n":"` +
			base64.RawURLEncoding.EncodeToString(pub.N.Bytes()) + `","e":"` +
			base64.RawURLEncoding.EncodeToString(eBytes) + `"}]}`))
	}
}

func mintEnvelope(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	require.NoError(t, err)
	return signed
}

func validClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": "identity-context-svc",
		"aud": "zoiko-internal",
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"principal": map[string]any{
			"principal_id": "principal-xyz",
		},
		"tenant_id":       "tenant-abc",
		"legal_entity_id": "entity-001",
		"correlation_id":  "corr-001",
	}
}

func TestVerify_ValidToken_Returns200WithIdentityHeaders(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "principal-xyz", rec.Header().Get("X-Principal-Id"))
	assert.Equal(t, "tenant-abc", rec.Header().Get("X-Tenant-Id"))
	assert.Equal(t, "entity-001", rec.Header().Get("X-Legal-Entity-Id"))
	assert.Equal(t, "corr-001", rec.Header().Get("X-Correlation-Id"))
}

func TestVerify_MissingAuthorizationHeader_Returns401(t *testing.T) {
	h, _, _ := newTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_NonBearerAuthorizationHeader_Returns401(t *testing.T) {
	h, _, _ := newTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_ExpiredToken_Returns401(t *testing.T) {
	h, key, _ := newTestEnv(t)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-5 * time.Minute).Unix()
	tok := mintEnvelope(t, key, testKid, claims)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_WrongIssuer_Returns401(t *testing.T) {
	h, key, _ := newTestEnv(t)
	claims := validClaims()
	claims["iss"] = "some-other-issuer"
	tok := mintEnvelope(t, key, testKid, claims)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_WrongAudience_Returns401(t *testing.T) {
	h, key, _ := newTestEnv(t)
	claims := validClaims()
	claims["aud"] = "some-other-audience"
	tok := mintEnvelope(t, key, testKid, claims)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_SignedByUntrustedKey_Returns401(t *testing.T) {
	h, _, _ := newTestEnv(t)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tok := mintEnvelope(t, otherKey, testKid, validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_UnknownKid_Returns401(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, "some-other-kid", validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestVerify_MissingPrincipalID_Returns401(t *testing.T) {
	h, key, _ := newTestEnv(t)
	claims := validClaims()
	claims["principal"] = map[string]any{"principal_id": ""}
	tok := mintEnvelope(t, key, testKid, claims)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
