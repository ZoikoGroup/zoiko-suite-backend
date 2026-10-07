package idempotency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/store"
)

// fakeStore is an in-memory stand-in with the same claim semantics as the
// Postgres implementation: first writer wins, a repeat with the same
// fingerprint replays, a repeat with a different one is a mismatch.
type fakeStore struct {
	mu       sync.Mutex
	records  map[string]*store.IdempotencyRecord
	claimErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: map[string]*store.IdempotencyRecord{}}
}

func key(tenant, endpoint, k string) string { return tenant + "|" + endpoint + "|" + k }

func (f *fakeStore) ClaimIdempotencyKey(_ context.Context, tenantID, endpoint, k, fingerprint string) (*store.IdempotencyRecord, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.records[key(tenantID, endpoint, k)]
	if !ok {
		f.records[key(tenantID, endpoint, k)] = &store.IdempotencyRecord{RequestFingerprint: fingerprint, CreatedAt: time.Now()}
		return nil, nil
	}
	// Mirrors the ON CONFLICT ... DO UPDATE takeover of an abandoned claim.
	if existing.ResponseStatus == 0 && existing.RequestFingerprint == fingerprint &&
		time.Since(existing.CreatedAt) > store.IdempotencyInFlightLease {
		existing.CreatedAt = time.Now()
		return nil, nil
	}
	if existing.RequestFingerprint != fingerprint {
		return nil, store.ErrIdempotencyFingerprintMismatch
	}
	return existing, nil
}

func (f *fakeStore) CompleteIdempotencyKey(_ context.Context, tenantID, endpoint, k string, status int, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec, ok := f.records[key(tenantID, endpoint, k)]; ok {
		rec.ResponseStatus = status
		rec.ResponseBody = body
	}
	return nil
}

func (f *fakeStore) ReleaseIdempotencyKey(_ context.Context, tenantID, endpoint, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, key(tenantID, endpoint, k))
	return nil
}

// countingHandler records how many times the command actually executed, which
// is the entire question this middleware exists to answer.
type countingHandler struct {
	calls  int
	status int
	body   string
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls++
	if h.status == 0 {
		h.status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(h.body))
}

func request(path, key, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-Tenant-Id", "tenant-a")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	return r
}

func serve(s Store, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Middleware(s, zap.NewNop())(h).ServeHTTP(w, r)
	return w
}

// THE defect. POST /v1/context/support mints a break-glass elevation; a
// client that retried after a lost response minted a SECOND one, because the
// header was mandatory and read by nothing.
func TestReplayedCommandExecutesOnceAndReplaysTheFirstAnswer(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{status: http.StatusCreated, body: `{"support_context_id":"sc-1"}`}
	body := `{"tenant_id":"tenant-a","reason_code":"INCIDENT_RESPONSE"}`

	first := serve(s, h, request("/v1/context/support", "retry-1", body))
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Empty(t, first.Header().Get(headerReplayed))

	second := serve(s, h, request("/v1/context/support", "retry-1", body))

	assert.Equal(t, 1, h.calls, "a retried command must not mint a second grant")
	assert.Equal(t, http.StatusCreated, second.Code, "the replay must not promote 201 to 200")
	assert.JSONEq(t, `{"support_context_id":"sc-1"}`, second.Body.String())
	assert.Equal(t, "true", second.Header().Get(headerReplayed),
		"a caller must be able to tell a replay from a fresh execution")
}

// The case ErrCodeIdempotencyMismatch was declared for, and which no code path
// in the service could previously reach.
func TestSameKeyDifferentBodyIsRefused(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{body: `{}`}

	serve(s, h, request("/v1/context/support", "retry-1", `{"ttl_seconds":300}`))
	w := serve(s, h, request("/v1/context/support", "retry-1", `{"ttl_seconds":86400}`))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "IDEMPOTENCY_MISMATCH")
	assert.Equal(t, 1, h.calls, "the mismatching second command must not execute")
}

// A key is scoped per resource, so the same key against two different support
// contexts is two commands, not a collision.
func TestSameKeyDifferentResourceIsNotAReplay(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{body: `{}`}

	serve(s, h, request("/v1/context/support/sc-1/review", "k", `{}`))
	w := serve(s, h, request("/v1/context/support/sc-2/review", "k", `{}`))

	assert.Equal(t, 2, h.calls)
	assert.NotEqual(t, http.StatusConflict, w.Code)
}

// A 503 from a dependency is a reason to retry. Recording it would hand the
// caller that failure forever and turn a transient blip into a dead key.
func TestServerErrorsAreNotRecorded(t *testing.T) {
	s := newFakeStore()
	failing := &countingHandler{status: http.StatusServiceUnavailable, body: `{"error":"upstream"}`}

	serve(s, failing, request("/v1/context/support", "retry-1", `{}`))
	assert.Empty(t, s.records, "a 5xx must release the claim")

	succeeding := &countingHandler{status: http.StatusCreated, body: `{"ok":true}`}
	w := serve(s, succeeding, request("/v1/context/support", "retry-1", `{}`))

	assert.Equal(t, http.StatusCreated, w.Code, "the retry must be allowed to succeed")
	assert.Equal(t, 1, succeeding.calls)
}

// A 4xx IS terminal — the same bad request will be bad again — so it is
// recorded and replayed rather than re-executed.
func TestClientErrorsAreRecorded(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{status: http.StatusBadRequest, body: `{"error":"missing_field"}`}

	serve(s, h, request("/v1/context/support", "retry-1", `{}`))
	w := serve(s, h, request("/v1/context/support", "retry-1", `{}`))

	assert.Equal(t, 1, h.calls)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Recording a resolve response would put a signed identity envelope — a
// working credential — into a table with a retention period, to be handed back
// to anyone later presenting the same key. The replay protection would become
// the credential leak.
func TestCredentialMintingEndpointsAreExempt(t *testing.T) {
	for _, path := range []string{"/v1/authenticate", "/v1/context/resolve"} {
		s := newFakeStore()
		h := &countingHandler{status: http.StatusOK, body: `{"envelope_jwt":"a.b.c"}`}

		serve(s, h, request(path, "retry-1", `{}`))
		serve(s, h, request(path, "retry-1", `{}`))

		assert.Equal(t, 2, h.calls, path+" must not be deduplicated")
		assert.Empty(t, s.records, path+" must never have its response recorded")
	}
}

// Reads carry no key requirement and must not be touched.
func TestReadsAreUntouched(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{status: http.StatusOK, body: `{}`}

	r := httptest.NewRequest(http.MethodGet, "/v1/context/support/sc-1", nil)
	r.Header.Set("X-Tenant-Id", "tenant-a")
	r.Header.Set("Idempotency-Key", "k")

	serve(s, h, r)
	assert.Equal(t, 1, h.calls)
	assert.Empty(t, s.records)
}

// Fail closed. Executing a command that cannot be deduplicated is the exact
// behaviour being fixed, so a store outage refuses rather than waves it through.
func TestStoreOutageRefusesRatherThanExecuting(t *testing.T) {
	s := newFakeStore()
	s.claimErr = assert.AnError
	h := &countingHandler{body: `{}`}

	w := serve(s, h, request("/v1/context/support", "retry-1", `{}`))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Zero(t, h.calls, "a command that cannot be deduplicated must not run")
}

// Two concurrent first attempts: one executes, the other is told to retry
// rather than being handed an empty record as though it were an answer.
func TestConcurrentFirstAttemptIsToldToRetry(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{status: http.StatusCreated, body: `{"id":"sc-1"}`}

	// Claim without completing, as an in-flight request would leave it.
	_, err := s.ClaimIdempotencyKey(context.Background(), "tenant-a",
		"POST /v1/context/support", "retry-1", fingerprintOf("", `{}`))
	require.NoError(t, err)

	w := serve(s, h, request("/v1/context/support", "retry-1", `{}`))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "IDEMPOTENCY_IN_FLIGHT")
	assert.Zero(t, h.calls)
}

// ── Replay is bound to the principal (2026-09-28) ────────────────────────────

func asPrincipal(r *http.Request, principal string) *http.Request {
	r.Header.Set("X-Principal-Id", principal)
	return r
}

// A replay is answered before the handler, so before authorization. Keyed on
// the body alone, a second principal holding the first one's key and body was
// handed the first one's stored response — justification included.
func TestReplayIsNotServedToADifferentPrincipal(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{body: `{"support_context_id":"sc-1","justification":"customer outage"}`}

	serve(s, h, asPrincipal(request("/v1/context/support", "k", `{"x":1}`), "alice"))
	w := serve(s, h, asPrincipal(request("/v1/context/support", "k", `{"x":1}`), "mallory"))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "IDEMPOTENCY_MISMATCH")
	assert.NotContains(t, w.Body.String(), "customer outage", "another principal's stored response must never be replayed")
	assert.Empty(t, w.Header().Get("X-Idempotent-Replay"))
	assert.Equal(t, 1, h.calls)
}

func TestReplayIsStillServedToTheSamePrincipal(t *testing.T) {
	s := newFakeStore()
	h := &countingHandler{body: `{"support_context_id":"sc-1"}`}

	serve(s, h, asPrincipal(request("/v1/context/support", "k", `{"x":1}`), "alice"))
	w := serve(s, h, asPrincipal(request("/v1/context/support", "k", `{"x":1}`), "alice"))

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "true", w.Header().Get("X-Idempotent-Replay"))
	assert.Equal(t, 1, h.calls)
}

// ── A claim is never stranded in flight (2026-09-28) ─────────────────────────

// chi's Recoverer sits OUTSIDE this middleware in cmd/server. A panicking
// handler never returned to it, so the claim stayed at status 0 and every retry
// got IDEMPOTENCY_IN_FLIGHT until the 7-day retention purge.
func TestPanicReleasesTheClaimSoARetryExecutes(t *testing.T) {
	s := newFakeStore()
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	chain := middleware.Recoverer(Middleware(s, zap.NewNop())(panicky))

	w1 := httptest.NewRecorder()
	chain.ServeHTTP(w1, request("/v1/context/support", "k", `{"x":1}`))
	require.Equal(t, http.StatusInternalServerError, w1.Code, "Recoverer must still see the panic")

	ok := &countingHandler{body: `{"ok":true}`}
	w2 := serve(s, ok, request("/v1/context/support", "k", `{"x":1}`))
	assert.Equal(t, http.StatusCreated, w2.Code)
	assert.Equal(t, 1, ok.calls, "the retry must execute, not be told the dead attempt is in flight")
}

// A process that dies mid-command releases nothing. Its claim is taken over by
// an identical retry once the lease has passed.
func TestAbandonedClaimIsTakenOverAfterTheLease(t *testing.T) {
	s := newFakeStore()
	fp := fingerprintOf("", `{"x":1}`)
	s.records[key("tenant-a", "POST /v1/context/support", "k")] = &store.IdempotencyRecord{
		RequestFingerprint: fp, CreatedAt: time.Now().Add(-store.IdempotencyInFlightLease - time.Minute),
	}

	h := &countingHandler{body: `{"ok":true}`}
	w := serve(s, h, request("/v1/context/support", "k", `{"x":1}`))

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, 1, h.calls)
}

// Inside the lease the claim may belong to a live command — still in flight.
func TestLiveClaimInsideTheLeaseIsStillInFlight(t *testing.T) {
	s := newFakeStore()
	fp := fingerprintOf("", `{"x":1}`)
	s.records[key("tenant-a", "POST /v1/context/support", "k")] = &store.IdempotencyRecord{
		RequestFingerprint: fp, CreatedAt: time.Now().Add(-10 * time.Second),
	}

	h := &countingHandler{}
	w := serve(s, h, request("/v1/context/support", "k", `{"x":1}`))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "IDEMPOTENCY_IN_FLIGHT")
	assert.Zero(t, h.calls)
}
