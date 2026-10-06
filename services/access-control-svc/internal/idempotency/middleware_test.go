package idempotency

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/store"
)

type memStore struct {
	recs map[string]*store.IdempotencyRecord
}

func (m *memStore) ClaimIdempotencyKey(_ context.Context, tenant, endpoint, key, fp string) (*store.IdempotencyRecord, error) {
	k := tenant + "|" + endpoint + "|" + key
	if r, ok := m.recs[k]; ok {
		if r.RequestFingerprint != fp {
			return nil, store.ErrIdempotencyFingerprintMismatch
		}
		return r, nil
	}
	m.recs[k] = &store.IdempotencyRecord{RequestFingerprint: fp}
	return nil, nil
}
func (m *memStore) CompleteIdempotencyKey(_ context.Context, tenant, endpoint, key string, status int, body []byte) error {
	r := m.recs[tenant+"|"+endpoint+"|"+key]
	r.ResponseStatus, r.ResponseBody = status, body
	return nil
}
func (m *memStore) ReleaseIdempotencyKey(_ context.Context, tenant, endpoint, key string) error {
	delete(m.recs, tenant+"|"+endpoint+"|"+key)
	return nil
}

func TestIdempotencyKeyIsHonoured(t *testing.T) {
	s := &memStore{recs: map[string]*store.IdempotencyRecord{}}
	calls := 0
	status := http.StatusCreated
	h := Middleware(s, zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"role_definition_id":"r1"}`))
	}))
	send := func(principal, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/role-definitions/", bytes.NewBufferString(body))
		req.Header.Set("Idempotency-Key", "k1")
		req.Header.Set("X-Tenant-Id", "t1")
		req.Header.Set("X-Principal-Id", principal)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := send("p1", `{"a":1}`); rr.Code != http.StatusCreated || calls != 1 {
		t.Fatalf("first: %d calls=%d", rr.Code, calls)
	}
	rr := send("p1", `{"a":1}`)
	if rr.Code != http.StatusOK || calls != 1 || rr.Header().Get(HeaderReplayed) != "true" || rr.Body.String() != `{"role_definition_id":"r1"}` {
		t.Fatalf("replay must be 200 from the record without re-running: %d calls=%d %q", rr.Code, calls, rr.Body.String())
	}
	if rr := send("p1", `{"a":2}`); rr.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("same key, different body: want 409, got %d", rr.Code)
	}
	if rr := send("p2", `{"a":1}`); rr.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("same key and body from another principal: want 409, got %d", rr.Code)
	}
}

func TestServerErrorReleasesTheKey(t *testing.T) {
	s := &memStore{recs: map[string]*store.IdempotencyRecord{}}
	calls := 0
	h := Middleware(s, zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(`{}`))
		req.Header.Set("Idempotency-Key", "k")
		req.Header.Set("X-Tenant-Id", "t")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if calls != 2 {
		t.Fatalf("a 503 must not be replayed forever; handler ran %d times", calls)
	}
}
