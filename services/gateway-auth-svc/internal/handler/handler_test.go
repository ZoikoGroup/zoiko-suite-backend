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

	"zoiko.io/gateway-auth-svc/internal/carta"
	"zoiko.io/gateway-auth-svc/internal/config"
	"zoiko.io/gateway-auth-svc/internal/handler"
	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/siem"
)

const testKid = "test-key-1"

// newTestEnv spins up a throwaway RSA keypair and a fake JWKS server exposing
// its public half, mirroring the real relationship between gateway-auth-svc
// and identity-context-svc's /.well-known/jwks.json endpoint.
func newTestEnv(t *testing.T) (*handler.Handler, *rsa.PrivateKey, *config.Config) {
	t.Helper()
	return newTestEnvWithCartaDecision(t, "")
}

// newTestEnvWithCartaDecision wires a fake carta-svc that always answers with
// decision (or runs with no carta-svc at all, when decision == ""), so tests
// can prove the block/no-block behavior for each CARTA outcome without a
// real risk-scoring algorithm in the loop.
func newTestEnvWithCartaDecision(t *testing.T, decision string) (*handler.Handler, *rsa.PrivateKey, *config.Config) {
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

	if decision != "" {
		cartaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"decision":"` + decision + `","risk_level":"HIGH","trust_score":10,"risk_factors":["test"]}`))
		}))
		t.Cleanup(cartaSrv.Close)
		cfg.CartaServiceURL = cartaSrv.URL
	}
	cartaClient := carta.New(cfg.CartaServiceURL, log)

	// SIEM is left unconfigured in every test: streaming is fire-and-forget
	// and opt-in per tenant, so its absence must never affect these
	// assertions (see siem.Client's doc comment).
	siemClient := siem.New(cfg.SIEMServiceURL, "gateway-auth-svc", log)

	return handler.New(cfg, jwksClient, cartaClient, siemClient, log), privateKey, cfg
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

// TestVerify_CartaAllow_Returns200 proves a normal-risk request still passes
// straight through when CARTA is wired in and configured.
func TestVerify_CartaAllow_Returns200(t *testing.T) {
	h, key, _ := newTestEnvWithCartaDecision(t, "ALLOW")
	tok := mintEnvelope(t, key, testKid, validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "principal-xyz", rec.Header().Get("X-Principal-Id"))
}

// TestVerify_CartaStepUpMFA_StillReturns200 proves STEP_UP_MFA is logged/
// streamed but deliberately not blocking, since there is no step-up
// challenge flow downstream to redirect to yet (see the doc comment in
// handler.go on denyWithReason's call site).
func TestVerify_CartaStepUpMFA_StillReturns200(t *testing.T) {
	h, key, _ := newTestEnvWithCartaDecision(t, "STEP_UP_MFA")
	tok := mintEnvelope(t, key, testKid, validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestVerify_CartaIsolate_Returns403 and TestVerify_CartaDeny_Returns403
// prove the two decisions this platform DOES have an automated response for
// actually block the request, distinctly from a 401 (auth failure) — this is
// a 403 (authenticated, but denied) with the deciding factor surfaced on
// X-Carta-Decision for observability.
func TestVerify_CartaIsolate_Returns403(t *testing.T) {
	h, key, _ := newTestEnvWithCartaDecision(t, "ISOLATE")
	tok := mintEnvelope(t, key, testKid, validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "ISOLATE", rec.Header().Get("X-Carta-Decision"))
}

func TestVerify_CartaDeny_Returns403(t *testing.T) {
	h, key, _ := newTestEnvWithCartaDecision(t, "DENY")
	tok := mintEnvelope(t, key, testKid, validClaims())

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()

	h.Verify(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "DENY", rec.Header().Get("X-Carta-Decision"))
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
