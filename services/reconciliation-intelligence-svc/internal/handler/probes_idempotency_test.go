package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/idempotency"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// memIdem is an in-memory idempotency.Store with PgStore's semantics.
type memIdem struct {
	mu      sync.Mutex
	records map[string]*idempotency.Record
}

func (m *memIdem) Claim(_ context.Context, tenantID, endpoint, key, fp string) (*idempotency.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := tenantID + "|" + endpoint + "|" + key
	if r, ok := m.records[id]; ok {
		if r.RequestFingerprint != fp {
			return nil, idempotency.ErrFingerprintMismatch
		}
		c := *r
		return &c, nil
	}
	m.records[id] = &idempotency.Record{RequestFingerprint: fp}
	return nil, nil
}

func (m *memIdem) Complete(_ context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.records[tenantID+"|"+endpoint+"|"+key]
	r.ResponseStatus, r.ResponseBody = status, append([]byte(nil), body...)
	return nil
}

func (m *memIdem) Release(_ context.Context, tenantID, endpoint, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, tenantID+"|"+endpoint+"|"+key)
	return nil
}

func TestReadyz_PublicAndFailsWithoutDatabase(t *testing.T) {
	router := setupTestRouter(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil)) // no identity headers at all
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with no database must be 503, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must be public, got %d", rec.Code)
	}
}

func TestAnalyze_IdempotencyKeyReplaysInsteadOfReanalyzing(t *testing.T) {
	st := store.NewMemoryStore()
	router := newTestRouter(t, deps{store: st, idem: &memIdem{records: map[string]*idempotency.Record{}}})

	post := func(body []byte, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(body))
		req.Header.Set("Idempotency-Key", key)
		withEnvelope(req)
		req.Header.Set("X-Tenant-ID", "tenant-idem")
		req.Header.Set("X-Principal-Id", "principal-01")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	first := post(analyzeBody("LE-3003", runLE3003), "idem-analyze-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first analyze: %d %s", first.Code, first.Body.String())
	}
	second := post(analyzeBody("LE-3003", runLE3003), "idem-analyze-1")
	if second.Code != http.StatusCreated || second.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("replay: %d replay-header=%q", second.Code, second.Header().Get("X-Idempotent-Replay"))
	}
	var j1, j2 domain.ReconciliationJob
	_ = json.Unmarshal(first.Body.Bytes(), &j1)
	_ = json.Unmarshal(second.Body.Bytes(), &j2)
	if j1.ID == "" || j1.ID != j2.ID {
		t.Fatalf("replay returned job %q, first was %q", j2.ID, j1.ID)
	}
	jobs, err := st.ListJobs(context.Background(), "tenant-idem", "LE-3003", "", "", 0, 0)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("a replay must not create a second job: %d jobs, err=%v", len(jobs), err)
	}

	if conflict := post(analyzeBody("LE-3003", "another-run"), "idem-analyze-1"); conflict.Code != http.StatusConflict {
		t.Fatalf("same key, different body: want 409, got %d", conflict.Code)
	}
}
