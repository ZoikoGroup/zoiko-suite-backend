package handler_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/gateway-auth-svc/internal/admission"
	"zoiko.io/gateway-auth-svc/internal/carta"
	"zoiko.io/gateway-auth-svc/internal/config"
	"zoiko.io/gateway-auth-svc/internal/handler"
	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/router"
	"zoiko.io/gateway-auth-svc/internal/siem"
	"zoiko.io/gateway-auth-svc/internal/tenantctx"
)

// siemRecorder is a fake siem-integration-svc that remembers every event.
type siemRecorder struct {
	mu     sync.Mutex
	events []struct{ Type, Severity, Message string }
}

func (s *siemRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The client first asks which exporters the tenant has, and streams only
		// to ACTIVE ones.
		if r.URL.Path == "/v1/siem/exporters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"exp-1","status":"ACTIVE"}]}`))
			return
		}
		var body struct {
			EventType string `json:"event_type"`
			Severity  string `json:"severity"`
			Message   string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.events = append(s.events, struct{ Type, Severity, Message string }{body.EventType, body.Severity, body.Message})
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *siemRecorder) count(eventType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

func (s *siemRecorder) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

type admEnv struct {
	h    *handler.Handler
	key  *rsa.PrivateKey
	r    http.Handler
	siem *siemRecorder
	adm  handler.Admission
}

// newAdmEnv builds the real router around a handler with admission controls,
// a stub JWKS and a recording SIEM, so the tests exercise the same path
// Traefik's ForwardAuth does.
func newAdmEnv(t *testing.T, a handler.Admission) *admEnv {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksSrv := httptest.NewServer(jwksHandler(&key.PublicKey, testKid))
	t.Cleanup(jwksSrv.Close)
	rec := &siemRecorder{}
	siemSrv := httptest.NewServer(rec.handler())
	t.Cleanup(siemSrv.Close)

	cfg := &config.Config{ExpectedIssuer: "identity-context-svc", ExpectedAudience: "zoiko-internal",
		JWKSURL: jwksSrv.URL, JWKSCacheTTL: time.Minute, TenantContextTTL: time.Minute}
	log := zap.NewNop()
	jc := jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL)
	sc := siem.New(siemSrv.URL, "gateway-auth-svc", log)
	t.Cleanup(sc.Close)
	if a.Recorder == nil {
		a.Recorder = admission.NewRecorder(256, 0, nil)
	}
	h := handler.New(cfg, jc, carta.New("", log), sc, tenantctx.New("", time.Minute, time.Minute), log).UseAdmission(a)
	return &admEnv{h: h, key: key, r: router.New(h, jc, nil), siem: rec, adm: a}
}

func (e *admEnv) token(t *testing.T, tenant string) string {
	t.Helper()
	c := validClaims()
	c["tenant_id"] = tenant
	return "Bearer " + mintEnvelope(t, e.key, testKid, c)
}

func (e *admEnv) verify(headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, req)
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) apicontract.Problem {
	t.Helper()
	require.Equal(t, apicontract.ContentTypeProblem, rec.Header().Get("Content-Type"))
	var p apicontract.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- problem details ---------------------------------------------------------

func TestRefusalIsRFC9457WithStableCodeAndDecisionID(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})
	rec := e.verify(map[string]string{"X-Forwarded-Uri": "/v1/things?access_token=SECRET", "X-Correlation-Id": "corr-77"})

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "no_token", rec.Header().Get("X-Auth-Denial-Reason"), "the header consoles read must survive")
	p := decodeProblem(t, rec)
	assert.Equal(t, "auth_denied", p.Code)
	assert.Equal(t, "https://api.zoikosuite.com/problems/auth-denied", p.Type)
	assert.Equal(t, 401, p.Status)
	assert.False(t, p.Retryable)
	assert.Equal(t, "corr-77", p.CorrelationID)
	assert.Equal(t, "/v1/things", p.Instance, "the problem instance must not carry the query string")
	assert.NotContains(t, rec.Body.String(), "SECRET")
	assert.Equal(t, p.DecisionID, rec.Header().Get("X-Gateway-Decision-Id"))

	d, ok := e.h.Recorder().Get(p.DecisionID)
	require.True(t, ok, "every refusal is retrievable by its decision id")
	assert.Equal(t, "no_token", d.Outcome)
	assert.Equal(t, "/v1/things", d.Path)
	b, _ := json.Marshal(d)
	assert.NotContains(t, string(b), "SECRET", "evidence must not keep query strings or tokens")
	assert.NotContains(t, string(b), "127.0.0.1", "evidence must not keep raw client addresses")
}

func TestEveryAuthRefusalCarriesAProblemBody(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})
	for name, c := range map[string]struct {
		hdr    map[string]string
		status int
		code   string
		reason string
	}{
		"no token":      {map[string]string{}, 401, "auth_denied", "no_token"},
		"garbage token": {map[string]string{"Authorization": "Bearer nonsense"}, 401, "auth_denied", "invalid_token"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.verify(c.hdr)
			assert.Equal(t, c.status, rec.Code)
			assert.Equal(t, c.reason, rec.Header().Get("X-Auth-Denial-Reason"))
			assert.Equal(t, c.code, decodeProblem(t, rec).Code)
		})
	}
}

// INT-NP-001: a token for one tenant presented against another tenant's host
// is refused and nothing is routed.
func TestForgedTenantContextRefusedAsProblem_INT_NP_001(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})
	rec := e.verify(map[string]string{
		"Authorization": e.token(t, "tenant-abc"), "X-Zoiko-Resolved-Tenant-Id": "tenant-other",
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "context_invalid", decodeProblem(t, rec).Code)
	assert.Empty(t, rec.Header().Get("X-Principal-Id"), "a refused request must not carry identity headers forward")
	assert.Empty(t, rec.Header().Get("X-Tenant-Id"))
	assert.Empty(t, rec.Header().Get("X-Carta-Decision"), "a mismatch is not a risk-engine decision")
}

// ---- INT-NP-002 --------------------------------------------------------------

// A valid credential is admitted: the gateway does not decide domain
// permission. The decision to admit is counted; domain refusal is the owning
// service's own and appears in its own evidence.
func TestValidCredentialIsAdmittedAndCounted_INT_NP_002(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})
	rec := e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc"), "X-Forwarded-Uri": "/v1/payments"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "tenant-abc", rec.Header().Get("X-Tenant-Id"))
	u, ok := e.h.Recorder().UsageOf("tenant-abc")
	require.True(t, ok)
	assert.Equal(t, uint64(1), u.Allowed)
	assert.Empty(t, e.h.Recorder().ListDenied("tenant-abc", 0), "an admitted request is not a denial")
}

// ---- INT-NP-003 --------------------------------------------------------------

func TestUnsupportedAPIVersionRefusedWithoutFallback_INT_NP_003(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{Policy: admission.Policy{SupportedVersions: []string{"v1"}}})
	tok := e.token(t, "tenant-abc")

	rec := e.verify(map[string]string{"Authorization": tok, "X-Forwarded-Uri": "/v2/payments"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	p := decodeProblem(t, rec)
	assert.Equal(t, "version_unsupported", p.Code)
	assert.Equal(t, "version_unsupported", rec.Header().Get("X-Auth-Denial-Reason"))
	assert.Empty(t, rec.Header().Get("X-Tenant-Id"), "no identity may be forwarded for a refused version")
	d, _ := e.h.Recorder().Get(p.DecisionID)
	assert.Equal(t, "v2", d.APIVersion)

	assert.Equal(t, http.StatusOK, e.verify(map[string]string{"Authorization": tok, "X-Forwarded-Uri": "/v1/payments"}).Code)
	assert.Equal(t, http.StatusOK, e.verify(map[string]string{"Authorization": tok, "X-Forwarded-Uri": "/unversioned/thing"}).Code)
	assert.Equal(t, http.StatusBadRequest,
		e.verify(map[string]string{"Authorization": tok, "X-Forwarded-Uri": "/v1/x", "X-API-Version": "v2"}).Code,
		"a header naming another version must not be quietly reconciled")
}

// ---- INT-NP-004 --------------------------------------------------------------

func TestOversizedAndCompressedBodiesRefusedBeforeAuthentication_INT_NP_004(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{Policy: admission.Policy{MaxBodyBytes: 1000}})

	// No Authorization header at all: if the policy ran after authentication
	// this would be a 401. 413 proves it is checked first.
	rec := e.verify(map[string]string{"Content-Length": "5000"})
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "schema_mismatch", decodeProblem(t, rec).Code)
	assert.Equal(t, "request_too_large", rec.Header().Get("X-Auth-Denial-Reason"))

	rec = e.verify(map[string]string{"Content-Length": "100", "Content-Encoding": "gzip"})
	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code, "a compressed body is refused: its expansion cannot be bounded here")
	assert.Equal(t, "request_encoding_refused", rec.Header().Get("X-Auth-Denial-Reason"))

	rec = e.verify(map[string]string{"Content-Length": "not-a-number"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "malformed_request", rec.Header().Get("X-Auth-Denial-Reason"))

	rec = e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc"), "Content-Length": "999"})
	assert.Equal(t, http.StatusOK, rec.Code, "a body inside the cap is admitted")
}

// ---- INT-NP-005 --------------------------------------------------------------

func TestConcurrentRequestsCannotBypassClientQuota_INT_NP_005(t *testing.T) {
	// rate ~0: nothing refills during the test, so exactly `burst` requests may
	// pass the limiter however the goroutines interleave.
	const burst = 10
	e := newAdmEnv(t, handler.Admission{IPLimiter: admission.NewLimiter(0.0001, burst, 0, nil)})

	var wg sync.WaitGroup
	var limited, passed int64
	start := make(chan struct{})
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := e.verify(map[string]string{"X-Forwarded-For": "198.51.100.7"}) // no token: a cheap request
			switch rec.Code {
			case http.StatusTooManyRequests:
				atomic.AddInt64(&limited, 1)
				assert.NotEmpty(t, rec.Header().Get("Retry-After"))
			case http.StatusUnauthorized:
				atomic.AddInt64(&passed, 1)
			default:
				t.Errorf("unexpected status %d", rec.Code)
			}
		}()
	}
	close(start)
	wg.Wait()
	assert.EqualValues(t, burst, passed, "exactly the burst may reach authentication")
	assert.EqualValues(t, 120-burst, limited)
}

func TestRateLimitedResponseIsProblemWithRetryAfter(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{IPLimiter: admission.NewLimiter(0.5, 1, 0, nil)})
	assert.Equal(t, http.StatusUnauthorized, e.verify(map[string]string{}).Code)
	rec := e.verify(map[string]string{})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	p := decodeProblem(t, rec)
	assert.Equal(t, "rate_limited", p.Code)
	assert.True(t, p.Retryable)
	assert.Equal(t, "rate_limited", rec.Header().Get("X-Auth-Denial-Reason"))
	assert.Equal(t, "2", rec.Header().Get("Retry-After"), "1 token short at 0.5/s is 2 whole seconds")
}

func TestClientsHaveSeparateQuotas(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{IPLimiter: admission.NewLimiter(0.0001, 1, 0, nil)})
	assert.Equal(t, 401, e.verify(map[string]string{"X-Forwarded-For": "198.51.100.1"}).Code)
	assert.Equal(t, 429, e.verify(map[string]string{"X-Forwarded-For": "198.51.100.1"}).Code)
	assert.Equal(t, 401, e.verify(map[string]string{"X-Forwarded-For": "198.51.100.2"}).Code,
		"one client's exhaustion must not refuse another")
}

func TestTenantQuotaAppliesAfterAuthenticationAndIsPerTenant(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{TenantLimiter: admission.NewLimiter(0.0001, 3, 0, nil)})
	tok := e.token(t, "tenant-abc")
	for i := 0; i < 3; i++ {
		require.Equal(t, 200, e.verify(map[string]string{"Authorization": tok}).Code)
	}
	rec := e.verify(map[string]string{"Authorization": tok})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "rate_limited", decodeProblem(t, rec).Code)
	assert.Empty(t, rec.Header().Get("X-Principal-Id"))

	assert.Equal(t, 200, e.verify(map[string]string{"Authorization": e.token(t, "tenant-xyz")}).Code,
		"another tenant has its own quota")
	u, _ := e.h.Recorder().UsageOf("tenant-abc")
	assert.EqualValues(t, 3, u.Allowed)
	assert.EqualValues(t, 1, u.RateLimited)
}

// ---- overload shedding (INT-BP-016, INT-NP-044) ------------------------------

func TestOverloadIsShedWithBackpressureNotQueued_INT_NP_044(t *testing.T) {
	gate := admission.NewGate(2)
	e := newAdmEnv(t, handler.Admission{Gate: gate})
	require.True(t, gate.Acquire())
	require.True(t, gate.Acquire()) // two requests "in flight"

	rec := e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc")})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	p := decodeProblem(t, rec)
	assert.Equal(t, "backpressure", p.Code)
	assert.True(t, p.Retryable)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Equal(t, "overloaded", rec.Header().Get("X-Auth-Denial-Reason"))
	d, ok := e.h.Recorder().Get(p.DecisionID)
	require.True(t, ok, "a shed request still leaves durable-enough evidence: it is never silently dropped")
	assert.Equal(t, "overloaded", d.Outcome)

	gate.Release()
	assert.Equal(t, 200, e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc")}).Code,
		"capacity returning must restore service")
	assert.EqualValues(t, 1, gate.InFlight(), "a served request must release its slot")
}

func TestSlotIsReleasedOnEveryPath(t *testing.T) {
	gate := admission.NewGate(5)
	e := newAdmEnv(t, handler.Admission{Gate: gate, Policy: admission.Policy{MaxBodyBytes: 10}})
	e.verify(map[string]string{})                                          // 401
	e.verify(map[string]string{"Content-Length": "999"})                   // policy refusal
	e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc")}) // 200
	e.verify(map[string]string{"Authorization": "Bearer junk"})            // 401
	assert.EqualValues(t, 0, gate.InFlight(), "no path may leak a concurrency slot")
}

// ---- events ------------------------------------------------------------------

func TestTenantRateLimitStreamsOneThrottledCanonicalEvent(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{TenantLimiter: admission.NewLimiter(0.0001, 1, 0, nil)})
	tok := e.token(t, "tenant-abc")
	require.Equal(t, 200, e.verify(map[string]string{"Authorization": tok}).Code)
	for i := 0; i < 25; i++ {
		require.Equal(t, 429, e.verify(map[string]string{"Authorization": tok}).Code)
	}
	waitFor(t, "api_rate_limited", func() bool { return e.siem.count("api_rate_limited") >= 1 })
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, e.siem.count("api_rate_limited"),
		"25 refusals in a burst must raise one event, not 25 (the metrics and recorder still count each)")
	u, _ := e.h.Recorder().UsageOf("tenant-abc")
	assert.EqualValues(t, 25, u.RateLimited)
}

// SIEM export is tenant-scoped; a refusal before authentication has no tenant
// and must not invent one.
func TestPreAuthenticationRefusalsAreNotStreamedToSIEM(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{Policy: admission.Policy{SupportedVersions: []string{"v1"}}})
	e.verify(map[string]string{"X-Forwarded-Uri": "/v9/x"})
	e.verify(map[string]string{})
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 0, e.siem.total())
	assert.Len(t, e.h.Recorder().ListDenied("", 0), 2, "but both are recorded")
}

func TestRefusalWithDedicatedEventDoesNotDoubleStream(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})
	e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc"), "X-Zoiko-Resolved-Tenant-Id": "tenant-other"})
	waitFor(t, "mismatch event", func() bool { return e.siem.count("auth.tenant_hostname_mismatch") == 1 })
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, e.siem.total(), "the dedicated event is the evidence; no generic duplicate")
}

func TestAbuseThresholdRaisesOneEventPerClientPerWindow(t *testing.T) {
	// A client that keeps being refused for its tenant is abusive: the event is
	// tenant-scoped so it reaches SIEM, and fires once, not on every refusal.
	rec := admission.NewRecorder(256, 5, nil)
	e := newAdmEnv(t, handler.Admission{Recorder: rec, TenantLimiter: admission.NewLimiter(0.0001, 1, 0, nil)})
	tok := e.token(t, "tenant-abc")
	for i := 0; i < 20; i++ {
		e.verify(map[string]string{"Authorization": tok, "X-Forwarded-For": "198.51.100.9"})
	}
	waitFor(t, "api_abuse_threshold_exceeded", func() bool { return e.siem.count("api_abuse_threshold_exceeded") >= 1 })
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, e.siem.count("api_abuse_threshold_exceeded"), "once per client per window")

	var refused int
	for _, d := range rec.ListDenied("tenant-abc", 0) {
		if d.ClientKey == admission.ClientKey("198.51.100.9") {
			refused++
		}
	}
	assert.Equal(t, 19, refused, "every refusal is still recorded")
}

// ---- the zero value stays inert ------------------------------------------------

func TestWithoutAdmissionNothingIsRefusedForAdmissionReasons(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})
	for i := 0; i < 200; i++ {
		rec := e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc"), "Content-Length": "99999999999", "X-Forwarded-Uri": "/v9/x"})
		require.Equal(t, 200, rec.Code, "an uninstalled control must not refuse anything")
	}
}

func TestDecisionsAreRecordedForClaims(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{TenantLimiter: admission.NewLimiter(0.0001, 1, 0, nil)})
	t1 := e.token(t, "tenant-abc")
	e.verify(map[string]string{"Authorization": t1})
	rec := e.verify(map[string]string{"Authorization": t1, "X-Forwarded-Uri": "/v1/ledger/journals?x=1", "X-Correlation-Id": "corr-test-123"})
	require.Equal(t, 429, rec.Code)
	d, ok := e.h.Recorder().Get(decodeProblem(t, rec).DecisionID)
	require.True(t, ok)
	assert.Equal(t, "tenant-abc", d.TenantID)
	assert.Equal(t, "principal-xyz", d.PrincipalID)
	assert.Equal(t, "/v1/ledger/journals", d.Path)
	assert.Equal(t, "POST", d.Method)
	assert.Equal(t, "corr-test-123", d.CorrelationID, "every decision is correlated")
}

// Regression, found by running the real image: with no X-Forwarded-For the
// client address is the peer's RemoteAddr, whose port changes with every
// connection. A quota keyed on it never limited anyone.
func TestQuotaIsPerHostNotPerConnection(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{IPLimiter: admission.NewLimiter(0.0001, 3, 0, nil)})
	codes := make([]int, 0, 8)
	for port := 40000; port < 40008; port++ {
		req := httptest.NewRequest(http.MethodGet, "/verify", nil)
		req.RemoteAddr = "192.0.2.50:" + strconv.Itoa(port)
		rec := httptest.NewRecorder()
		e.r.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	var passed, limited int
	for _, c := range codes {
		switch c {
		case http.StatusUnauthorized:
			passed++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	assert.Equal(t, 3, passed, "one host, three tokens, however many connections: %v", codes)
	assert.Equal(t, 5, limited)

	other := httptest.NewRequest(http.MethodGet, "/verify", nil)
	other.RemoteAddr = "192.0.2.51:40000"
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, other)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a different host has its own quota")
}

// INT-NP-002 end to end: the gateway admits a valid credential and forwards identity, the owning service refuses the
// command, and the two decisions stay separately retained and joinable by request id. The gateway neither grants the
// permission nor overrides the refusal.
func TestGatewayAdmitsAndTheDomainRefusesWithBothDecisionsRetained_INT_NP_002(t *testing.T) {
	e := newAdmEnv(t, handler.Admission{})

	// the owning service: it trusts only the identity the gateway forwarded and decides permission itself
	type domainDecision struct{ requestID, principal, outcome string }
	var domainEvidence []domainDecision
	domain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := r.Header.Get("X-Principal-Id") == "principal-with-permission"
		out := "DENIED"
		if allowed {
			out = "GRANTED"
		}
		domainEvidence = append(domainEvidence, domainDecision{r.Header.Get("X-Request-Id"), r.Header.Get("X-Principal-Id"), out})
		if !allowed {
			w.WriteHeader(http.StatusForbidden)
		}
	})

	rec := e.verify(map[string]string{"Authorization": e.token(t, "tenant-abc"), "X-Forwarded-Uri": "/v1/payments", "X-Request-Id": "req-np002"})
	require.Equal(t, http.StatusOK, rec.Code, "the gateway admits a valid credential")
	assert.Empty(t, rec.Header().Get("X-Permissions"), "the gateway grants no domain permission")

	// the proxy forwards what the gateway returned
	fwd := httptest.NewRequest(http.MethodPost, "/v1/payments", nil)
	for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Correlation-Id", "X-Request-Id"} {
		fwd.Header.Set(h, rec.Header().Get(h))
	}
	dr := httptest.NewRecorder()
	domain.ServeHTTP(dr, fwd)
	assert.Equal(t, http.StatusForbidden, dr.Code, "the owning service refuses; nothing upstream can turn that into success")

	// both decisions exist, separately, and join on the request id
	u, ok := e.h.Recorder().UsageOf("tenant-abc")
	require.True(t, ok)
	assert.Equal(t, uint64(1), u.Allowed, "the gateway's admission is retained")
	require.Len(t, domainEvidence, 1)
	assert.Equal(t, "DENIED", domainEvidence[0].outcome)
	assert.Equal(t, "req-np002", domainEvidence[0].requestID, "the domain's decision carries the id the gateway echoed")
	assert.Equal(t, rec.Header().Get("X-Request-Id"), domainEvidence[0].requestID)
}
