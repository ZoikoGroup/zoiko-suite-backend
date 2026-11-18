package handler_test

// ER-5: the identity gate in front of this service, in the mode compose runs
// it (warn). warn admits a caller that sends no envelope (the callers in other
// domains send none yet, D-ER-6), but an envelope that is present must verify:
// a tampered or expired one is 401, and so is one whose session identity-
// context reports revoked. Review focus #5.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

var gateKey, _ = rsa.GenerateKey(rand.Reader, 2048)

func signedEnvelope(t *testing.T, mutate func(jwt.MapClaims)) string {
	t.Helper()
	now := time.Now()
	m := jwt.MapClaims{
		"iss": "identity-context-svc", "aud": "evidence-requirements-svc",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": "j-" + t.Name(),
		"tenant_id":             testTenant,
		"principal":             map[string]any{"principal_id": "verified-principal", "tenant_id": testTenant},
		"session_trust_posture": map[string]any{"session_context_id": "sc-1", "mfa_verified": true},
		"auth_time":             now.Add(-time.Minute).Unix(),
	}
	if mutate != nil {
		mutate(m)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, m)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(gateKey)
	require.NoError(t, err)
	return s
}

// gatedRouter is the production middleware order (main.go): the identity
// gate, then tenant context, then the service input contract, then routes.
func gatedRouter(t *testing.T, live *atomic.Bool, az *recordingAuthz) http.Handler {
	t.Helper()
	v, err := idenv.NewVerifier(idenv.VerifierOptions{Keys: idenv.StaticKeys{"k1": &gateKey.PublicKey},
		Issuer: "identity-context-svc", Audience: "evidence-requirements-svc"})
	require.NoError(t, err)
	status := idenv.StatusFunc(func(context.Context, string, *idenv.Claims) (idenv.SessionStatus, error) {
		if live.Load() {
			return idenv.SessionStatus{Active: true}, nil
		}
		return idenv.SessionStatus{Active: false, Reason: "LOGOUT"}, nil
	})
	gate, err := idenv.NewGate(idenv.GateOptions{Service: "evidence-requirements-svc", Mode: idenv.ModeWarn, Verifier: v,
		Revocation: idenv.NewRevocation(idenv.RevocationOptions{Source: status, ActiveTTL: time.Millisecond}),
		PeerName:   func(*http.Request) string { return "" }})
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(gate.Middleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(&stubStore{summaryResult: &domain.EvidenceSummary{}}, az, &stubDocs{}, zap.NewNop()).WithCursors(testCursors)
	handler.RegisterRoutes(r, h)
	return r
}

func gatedGet(h http.Handler, bearer, forgedTenant, forgedPrincipal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/evidence/summary", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if forgedTenant != "" {
		req.Header.Set("X-Tenant-Id", forgedTenant)
	}
	if forgedPrincipal != "" {
		req.Header.Set("X-Principal-Id", forgedPrincipal)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestER05_VerifiedEnvelopeIsTheCaller(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	az := &recordingAuthz{}
	h := gatedRouter(t, &live, az)

	rec := gatedGet(h, signedEnvelope(t, nil), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"EVIDENCE_REQUIREMENT_READ@PLATFORM"}, az.asked)
}

func TestER05_TamperedOrExpiredEnvelopeIs401EvenInWarnMode(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	h := gatedRouter(t, &live, &recordingAuthz{})

	good := signedEnvelope(t, nil)
	tampered := good[:len(good)-4] + "AAAA"
	rec := gatedGet(h, tampered, "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a bad signature must never be admitted")

	expired := signedEnvelope(t, func(m jwt.MapClaims) {
		m["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		m["exp"] = time.Now().Add(-time.Hour).Unix()
	})
	rec = gatedGet(h, expired, "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "an expired envelope must never be admitted")
}

// A session invalidated at identity-context is refused on the next call
// after the revocation cache's TTL (AU-01).
func TestER05_RevokedSessionIsRefused(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	h := gatedRouter(t, &live, &recordingAuthz{})
	tok := signedEnvelope(t, nil)

	require.Equal(t, http.StatusOK, gatedGet(h, tok, "", "").Code)
	live.Store(false)
	time.Sleep(5 * time.Millisecond) // past ActiveTTL
	rec := gatedGet(h, tok, "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "session_revoked")
}

// A client cannot assert another identity next to a verified envelope.
func TestER05_ForgedIdentityHeadersAreRefused(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	h := gatedRouter(t, &live, &recordingAuthz{})
	rec := gatedGet(h, signedEnvelope(t, nil), "", "someone-else")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = gatedGet(h, signedEnvelope(t, nil), otherTenant, "")
	assert.NotEqual(t, http.StatusOK, rec.Code, "a forged X-Tenant-Id next to a verified envelope must not be served")
}
