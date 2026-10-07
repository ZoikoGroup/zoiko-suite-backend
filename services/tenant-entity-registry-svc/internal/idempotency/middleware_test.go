package idempotency_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/idempotency"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

// memStore mirrors PgStore's claim semantics.
type memStore struct {
	mu   sync.Mutex
	recs map[string]*store.IdempotencyRecord
}

func newMemStore() *memStore { return &memStore{recs: map[string]*store.IdempotencyRecord{}} }

func k(t, e, key string) string { return t + "|" + e + "|" + key }

func (m *memStore) ClaimIdempotencyKey(_ context.Context, t, e, key, fp string) (*store.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.recs[k(t, e, key)]; ok {
		if r.RequestFingerprint != fp {
			return nil, store.ErrIdempotencyFingerprintMismatch
		}
		cp := *r
		return &cp, nil
	}
	m.recs[k(t, e, key)] = &store.IdempotencyRecord{RequestFingerprint: fp}
	return nil, nil
}

func (m *memStore) CompleteIdempotencyKey(_ context.Context, t, e, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.recs[k(t, e, key)]
	r.ResponseStatus, r.ResponseBody = status, append([]byte(nil), body...)
	return nil
}

func (m *memStore) ReleaseIdempotencyKey(_ context.Context, t, e, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.recs[k(t, e, key)]; r != nil && r.ResponseStatus == 0 {
		delete(m.recs, k(t, e, key))
	}
	return nil
}

// counting is a command handler that records how often it actually ran.
type counting struct {
	runs   int
	status int
}

func (c *counting) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.runs++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.status)
	_, _ = w.Write([]byte(`{"record_version":2}`))
}

func send(h http.Handler, key, principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/t-1/commands/SuspendTenant", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	ctx := domain.WithPrincipal(domain.WithTenant(req.Context(), "t-1"), principal)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func TestRetryIsAnsweredFromTheFirstResponseAndRunsOnce(t *testing.T) {
	cmd := &counting{status: http.StatusOK}
	h := idempotency.Middleware(newMemStore(), zap.NewNop())(cmd)

	first := send(h, "key-1", "p-1", `{"reason":"incident"}`)
	retry := send(h, "key-1", "p-1", `{"reason":"incident"}`)

	assert.Equal(t, 1, cmd.runs, "a retry must not issue the command a second time")
	assert.Equal(t, first.Code, retry.Code)
	assert.JSONEq(t, first.Body.String(), retry.Body.String())
	assert.Equal(t, "true", retry.Header().Get("X-Idempotent-Replay"))
	assert.Empty(t, first.Header().Get("X-Idempotent-Replay"))
}

func TestSameKeyDifferentRequestIsRefused(t *testing.T) {
	cmd := &counting{status: http.StatusOK}
	h := idempotency.Middleware(newMemStore(), zap.NewNop())(cmd)

	send(h, "key-1", "p-1", `{"reason":"incident"}`)
	other := send(h, "key-1", "p-1", `{"reason":"something else"}`)

	assert.Equal(t, http.StatusConflict, other.Code)
	assert.Equal(t, 1, cmd.runs)
}

// A second principal presenting the first one's key and body must not be
// handed the first one's stored response: replay runs before authorization.
func TestAnotherPrincipalCannotReplaySomeoneElsesResponse(t *testing.T) {
	cmd := &counting{status: http.StatusOK}
	h := idempotency.Middleware(newMemStore(), zap.NewNop())(cmd)

	send(h, "key-1", "p-1", `{"reason":"incident"}`)
	thief := send(h, "key-1", "p-2", `{"reason":"incident"}`)

	assert.Equal(t, http.StatusConflict, thief.Code)
	assert.NotContains(t, thief.Body.String(), "record_version")
}

func TestServerErrorIsNotRecordedSoARetryRetries(t *testing.T) {
	cmd := &counting{status: http.StatusServiceUnavailable}
	h := idempotency.Middleware(newMemStore(), zap.NewNop())(cmd)

	send(h, "key-1", "p-1", `{}`)
	cmd.status = http.StatusOK
	retry := send(h, "key-1", "p-1", `{}`)

	assert.Equal(t, 2, cmd.runs, "a 503 is transient; the retry must genuinely run")
	assert.Equal(t, http.StatusOK, retry.Code)
}

func TestReadsAndUnkeyedRequestsPassThrough(t *testing.T) {
	cmd := &counting{status: http.StatusOK}
	h := idempotency.Middleware(newMemStore(), zap.NewNop())(cmd)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/tenants/t-1", nil)
		req.Header.Set("Idempotency-Key", "key-1")
		h.ServeHTTP(httptest.NewRecorder(), req.WithContext(domain.WithTenant(req.Context(), "t-1")))
	}
	require.Equal(t, 2, cmd.runs, "a GET is never deduplicated")

	// No verified tenant: nothing to scope a record to.
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "key-2")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, 3, cmd.runs)
}
