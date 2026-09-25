package handler_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
	"zoiko.io/gateway-auth-svc/internal/telemetry"
	"zoiko.io/gateway-auth-svc/internal/tenantctx"
)

// recordingMetrics captures what the handler reported, so a test can assert on
// the SIGNAL rather than only on the status code.
//
// This matters more here than it sounds. /verify answers 401 for a missing
// bearer token, for an expired token, for a forged signature and for a total
// JWKS outage — four situations with four different responses required of an
// operator, behind one status code on one route. The outcome label is the only
// thing that separates them, so a test that checks only the code cannot tell
// whether the separation works.
type recordingMetrics struct {
	mu        sync.Mutex
	outcomes  []string
	jwksOps   []string
	tenantCtx []string
	carta     []string
}

func (m *recordingMetrics) VerifyDecision(o string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, o)
}

func (m *recordingMetrics) JWKSError(op string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jwksOps = append(m.jwksOps, op)
}

func (m *recordingMetrics) TenantContext(o string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenantCtx = append(m.tenantCtx, o)
}

func (m *recordingMetrics) CartaDecision(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.carta = append(m.carta, d)
}

func (m *recordingMetrics) snapshot() ([]string, []string, []string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.outcomes...),
		append([]string(nil), m.jwksOps...),
		append([]string(nil), m.tenantCtx...),
		append([]string(nil), m.carta...)
}

// TestVerify_EveryTerminalPathRecordsItsOutcome walks the outcomes reachable
// without a stub registry and asserts each reports itself distinctly.
func TestVerify_EveryTerminalPathRecordsItsOutcome(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T, key *rsa.PrivateKey) *http.Request
		want    string
		wantWWW int
	}{
		{
			name: "no credential at all",
			build: func(t *testing.T, _ *rsa.PrivateKey) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/verify", nil)
			},
			want:    telemetry.OutcomeNoToken,
			wantWWW: http.StatusUnauthorized,
		},
		{
			name: "a credential that does not verify",
			build: func(t *testing.T, key *rsa.PrivateKey) *http.Request {
				claims := validClaims()
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
				req := httptest.NewRequest(http.MethodGet, "/verify", nil)
				req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, claims))
				return req
			},
			want:    telemetry.OutcomeInvalidToken,
			wantWWW: http.StatusUnauthorized,
		},
		{
			name: "verified, but the envelope names no principal",
			build: func(t *testing.T, key *rsa.PrivateKey) *http.Request {
				claims := validClaims()
				claims["principal"] = map[string]any{"principal_id": ""}
				req := httptest.NewRequest(http.MethodGet, "/verify", nil)
				req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, claims))
				return req
			},
			// Distinct from invalid_token on purpose: the signature was good,
			// so this is identity-context-svc minting a malformed envelope and
			// the fix is upstream, not with the caller.
			want:    telemetry.OutcomeIncompleteClaims,
			wantWWW: http.StatusUnauthorized,
		},
		{
			name: "a good token against another tenant's hostname",
			build: func(t *testing.T, key *rsa.PrivateKey) *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/verify", nil)
				req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
				req.Header.Set("X-Zoiko-Resolved-Tenant-Id", "tenant-somebody-else")
				return req
			},
			want:    telemetry.OutcomeTenantHostnameMismatch,
			wantWWW: http.StatusForbidden,
		},
		{
			name: "everything in order",
			build: func(t *testing.T, key *rsa.PrivateKey) *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/verify", nil)
				req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
				return req
			},
			want:    telemetry.OutcomeAllowed,
			wantWWW: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, key, _ := newTestEnv(t)
			m := &recordingMetrics{}
			h.UseMetrics(m)

			rec := httptest.NewRecorder()
			h.Verify(rec, tc.build(t, key))

			assert.Equal(t, tc.wantWWW, rec.Code)
			outcomes, _, _, _ := m.snapshot()
			require.Len(t, outcomes, 1, "exactly one terminal outcome per request")
			assert.Equal(t, tc.want, outcomes[0])
			// The reason reaches the caller too. Traefik returns an
			// unsuccessful ForwardAuth reply verbatim, so this is what lets a
			// console tell the gateway's refusal apart from the backend's.
			if tc.wantWWW != http.StatusOK {
				assert.Equal(t, tc.want, rec.Header().Get("X-Auth-Denial-Reason"))
			}
		})
	}
}

// TestVerify_JWKSOutageIsNotReportedAsABadToken pins the distinction that the
// unstructured error strings used to hide.
//
// A JWKS fetch failure and a forged signature both answer 401 — correctly, the
// caller cannot be verified either way. But one means identity-context-svc is
// down and EVERY request in the estate is failing, and the other means one
// caller sent one bad token. Reported identically, the outage looks like a
// credential-stuffing attempt and the real fault goes unlooked-for.
func TestVerify_JWKSOutageIsNotReportedAsABadToken(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	// A JWKS endpoint that is reachable but broken — the shape an outage
	// actually takes behind a load balancer.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()

	cfg := &config.Config{
		ExpectedIssuer:   "identity-context-svc",
		ExpectedAudience: "zoiko-internal",
		JWKSCacheTTL:     time.Minute,
		JWKSURL:          down.URL,
	}
	log := zap.NewNop()
	h := handler.New(cfg, jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL),
		carta.New("", log), siem.New("", "gateway-auth-svc", log),
		tenantctx.New("", time.Minute, time.Minute), log)
	m := &recordingMetrics{}
	h.UseMetrics(m)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, privateKey, testKid, validClaims()))
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code, "the caller still cannot be verified")
	_, jwksOps, _, _ := m.snapshot()
	require.Len(t, jwksOps, 1, "a key-retrieval failure must be recorded as one")
	assert.Equal(t, "fetch", jwksOps[0], "an unreachable JWKS endpoint is a fetch failure, not a key miss")
}

// TestVerify_UnknownKidIsAKeyMissNotAnOutage is the other half: the endpoint
// answered fine and simply does not carry this kid, which is what a signing-key
// rotation looks like from here and self-heals.
func TestVerify_UnknownKidIsAKeyMissNotAnOutage(t *testing.T) {
	h, key, _ := newTestEnv(t)
	m := &recordingMetrics{}
	h.UseMetrics(m)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, "some-other-kid", validClaims()))
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	_, jwksOps, _, _ := m.snapshot()
	require.Len(t, jwksOps, 1)
	assert.Equal(t, "key_lookup", jwksOps[0])
}

// TestVerify_TenantHostnameMismatchStreamsToSIEM.
//
// This is the only refusal in this service that is evidence of an ATTACK
// rather than of a misconfiguration: by construction the token verified, so
// whoever presented it holds valid credentials, and they are using them
// against a tenant that is not theirs. It was reaching a log line and nothing
// else, while strictly less serious events — every CARTA decision, every
// tenant-context denial — were already streaming to SIEM.
func TestVerify_TenantHostnameMismatchStreamsToSIEM(t *testing.T) {
	type captured struct {
		eventType string
		severity  string
		message   string
	}
	got := make(chan captured, 4)

	siemSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/siem/exporters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"exp-1","status":"ACTIVE"}]}`))
			return
		}
		// The event is a JSON body on POST /v1/siem/stream, not query params.
		var body struct {
			EventType string `json:"event_type"`
			Severity  string `json:"severity"`
			Message   string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- captured{eventType: body.EventType, severity: body.Severity, message: body.Message}
		w.WriteHeader(http.StatusOK)
	}))
	defer siemSrv.Close()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksSrv := httptest.NewServer(jwksHandler(&privateKey.PublicKey, testKid))
	defer jwksSrv.Close()

	log := zap.NewNop()
	cfg := &config.Config{
		ExpectedIssuer:   "identity-context-svc",
		ExpectedAudience: "zoiko-internal",
		JWKSCacheTTL:     time.Minute,
		JWKSURL:          jwksSrv.URL,
		SIEMServiceURL:   siemSrv.URL,
	}
	siemClient := siem.New(cfg.SIEMServiceURL, "gateway-auth-svc", log)
	defer siemClient.Close()

	h := handler.New(cfg, jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL),
		carta.New("", log), siemClient, tenantctx.New("", time.Minute, time.Minute), log)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, privateKey, testKid, validClaims()))
	req.Header.Set("X-Zoiko-Resolved-Tenant-Id", "tenant-somebody-else")
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)

	select {
	case c := <-got:
		assert.Equal(t, "auth.tenant_hostname_mismatch", c.eventType)
		// CRITICAL, not HIGH: the credential is genuine. A tenant-context
		// denial is HIGH and is a caller asking for something it may not have;
		// this is a caller holding something it should not.
		assert.Equal(t, string(siem.SeverityCritical), c.severity)
		// Both tenants named, so the record says what was presented where.
		assert.Contains(t, c.message, "tenant-abc")
		assert.Contains(t, c.message, "tenant-somebody-else")
	case <-time.After(5 * time.Second):
		t.Fatal("token/hostname spoofing was not streamed to SIEM — it reaches a log line and nothing else")
	}
}

// TestBearerSchemeIsCaseInsensitive — RFC 7235 §2.1 defines auth-scheme as a
// case-insensitive token and RFC 6750 inherits that. A strict
// HasPrefix("Bearer ") refused "bearer <token>" with the same 401 as a forged
// credential, which is the least diagnosable way to fail a conformant client.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			h, key, _ := newTestEnv(t)
			req := httptest.NewRequest(http.MethodGet, "/verify", nil)
			req.Header.Set("Authorization", scheme+" "+mintEnvelope(t, key, testKid, validClaims()))
			rec := httptest.NewRecorder()
			h.Verify(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}

	// Still refused: a different scheme, and a scheme with no credential.
	for _, header := range []string{"Basic dXNlcjpwYXNz", "Bearer", "Bearer   ", "bearertoken"} {
		t.Run("refused/"+header, func(t *testing.T) {
			h, _, _ := newTestEnv(t)
			req := httptest.NewRequest(http.MethodGet, "/verify", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			h.Verify(rec, req)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

// TestVerify_CartaAllowIsStillCounted — ALLOW does not block, so
// before this it left no trace anywhere. STEP_UP_MFA is now blocked
// (returns 403) because there is no step-up flow downstream, and letting
// it pass silently defeats the risk engine's intent.
func TestVerify_CartaAllowIsStillCounted(t *testing.T) {
	h, key, _ := newTestEnvWithCartaDecision(t, "ALLOW")
	m := &recordingMetrics{}
	h.UseMetrics(m)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "ALLOW does not block")
	_, _, _, decisions := m.snapshot()
	require.Len(t, decisions, 1)
	assert.Equal(t, "ALLOW", decisions[0])
}

// TestVerify_CartaStepUpMFA_Returns403AndCounted proves STEP_UP_MFA is
// blocked and still counted in the metrics.
func TestVerify_CartaStepUpMFA_Returns403AndCounted(t *testing.T) {
	h, key, _ := newTestEnvWithCartaDecision(t, "STEP_UP_MFA")
	m := &recordingMetrics{}
	h.UseMetrics(m)

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "STEP_UP_MFA now blocks")
	_, _, _, decisions := m.snapshot()
	require.Len(t, decisions, 1)
	assert.Equal(t, "STEP_UP_MFA", decisions[0])
}

// TestOutcomeConstantsMatchTelemetry stops the handler's private copy of the
// outcome labels drifting from the exported set telemetry pre-initialises.
//
// The handler deliberately does not import internal/telemetry (see
// DomainMetrics), so the two lists are maintained separately. A label emitted
// by the handler but absent from telemetry's initialisation loop is a series
// that does not exist until the first time it fires — which, for the security
// outcomes, is exactly when an alert on it needs to already be evaluating.
func TestOutcomeConstantsMatchTelemetry(t *testing.T) {
	h, key, _ := newTestEnv(t)
	m := &recordingMetrics{}
	h.UseMetrics(m)

	known := map[string]bool{
		telemetry.OutcomeAllowed:                 true,
		telemetry.OutcomeNoToken:                 true,
		telemetry.OutcomeInvalidToken:            true,
		telemetry.OutcomeIncompleteClaims:        true,
		telemetry.OutcomeTenantHostnameMismatch:  true,
		telemetry.OutcomeCartaBlocked:            true,
		telemetry.OutcomeTenantContextDenied:     true,
		telemetry.OutcomeTenantContextUnresolved: true,
	}

	// Drive several paths, then assert every label the handler produced is one
	// telemetry knows about.
	reqs := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/verify", nil),
	}
	ok := httptest.NewRequest(http.MethodGet, "/verify", nil)
	ok.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	reqs = append(reqs, ok)

	bad := httptest.NewRequest(http.MethodGet, "/verify", nil)
	bad.Header.Set("Authorization", "Bearer not-a-token")
	reqs = append(reqs, bad)

	for _, req := range reqs {
		h.Verify(httptest.NewRecorder(), req)
	}

	outcomes, _, _, _ := m.snapshot()
	require.NotEmpty(t, outcomes)
	for _, o := range outcomes {
		assert.True(t, known[o], "handler emitted outcome %q, which telemetry does not pre-initialise", o)
	}
}

var _ handler.DomainMetrics = (*recordingMetrics)(nil)

var _ = jwt.MapClaims{}
