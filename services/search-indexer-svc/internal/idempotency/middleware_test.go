package idempotency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"zoiko.io/search-indexer-svc/internal/store"
)

// memStore is an in-memory Store with the PgStore's semantics.
type memStore struct {
	mu   sync.Mutex
	recs map[string]*store.IdempotencyRecord
}

func newMem() *memStore { return &memStore{recs: map[string]*store.IdempotencyRecord{}} }

func (m *memStore) ClaimIdempotencyKey(_ context.Context, tenant, endpoint, key, fp string) (*store.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tenant + "|" + endpoint + "|" + key
	if rec, ok := m.recs[k]; ok {
		if rec.RequestFingerprint != fp {
			return nil, store.ErrIdempotencyFingerprintMismatch
		}
		copy := *rec
		return &copy, nil
	}
	m.recs[k] = &store.IdempotencyRecord{RequestFingerprint: fp}
	return nil, nil
}
func (m *memStore) CompleteIdempotencyKey(_ context.Context, tenant, endpoint, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recs[tenant+"|"+endpoint+"|"+key]
	rec.ResponseStatus, rec.ResponseBody = status, append([]byte{}, body...)
	return nil
}
func (m *memStore) ReleaseIdempotencyKey(_ context.Context, tenant, endpoint, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, tenant+"|"+endpoint+"|"+key)
	return nil
}

type counter struct {
	n      int
	status int
}

func (c *counter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.n++
	status := c.status
	if status == 0 {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"export_id":"x"}`))
}

func send(h http.Handler, path, principal, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-Tenant-Id", "11111111-1111-1111-1111-111111111111")
	r.Header.Set("X-Principal-Id", principal)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The defect: a retried export recorded a second authorization.
func TestReplay_SecondIdenticalCommandIsAnsweredFromTheRecord(t *testing.T) {
	c := &counter{}
	h := Middleware(newMem(), zap.NewNop())(c)

	first := send(h, "/v1/search-exports", "p1", "k1", `{"scope":"s"}`)
	second := send(h, "/v1/search-exports", "p1", "k1", `{"scope":"s"}`)

	assert.Equal(t, 1, c.n, "the command ran once")
	assert.Equal(t, http.StatusAccepted, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get("X-Idempotent-Replay"))
}

func TestReplay_SameKeyDifferentBodyIsAMismatch(t *testing.T) {
	c := &counter{}
	h := Middleware(newMem(), zap.NewNop())(c)
	send(h, "/v1/search-exports", "p1", "k1", `{"scope":"s"}`)
	w := send(h, "/v1/search-exports", "p1", "k1", `{"scope":"other"}`)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "IDEMPOTENCY_MISMATCH")
	assert.Equal(t, 1, c.n)
}

// Another principal presenting the first one's key and body does not get the
// first one's stored response.
func TestReplay_IsBoundToThePrincipal(t *testing.T) {
	c := &counter{}
	h := Middleware(newMem(), zap.NewNop())(c)
	send(h, "/v1/search-exports", "p1", "k1", `{"scope":"s"}`)
	w := send(h, "/v1/search-exports", "p2", "k1", `{"scope":"s"}`)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Empty(t, w.Header().Get("X-Idempotent-Replay"))
}

// A 5xx is not the command's answer: the retry genuinely retries.
func TestReplay_ServerErrorReleasesTheClaim(t *testing.T) {
	c := &counter{status: http.StatusServiceUnavailable}
	h := Middleware(newMem(), zap.NewNop())(c)
	send(h, "/v1/index-contracts", "p1", "k1", `{}`)
	send(h, "/v1/index-contracts", "p1", "k1", `{}`)
	assert.Equal(t, 2, c.n)
}

// Searches are queries; a replayed search would freeze the first request's
// authorization (NP-56).
func TestReplay_QueriesAreNeverDeduplicated(t *testing.T) {
	for _, path := range []string{"/v1/search", "/v1/search/semantic", "/v1/retrieve"} {
		c := &counter{}
		h := Middleware(newMem(), zap.NewNop())(c)
		send(h, path, "p1", "k1", `{}`)
		send(h, path, "p1", "k1", `{}`)
		assert.Equal(t, 2, c.n, path)
	}
}
