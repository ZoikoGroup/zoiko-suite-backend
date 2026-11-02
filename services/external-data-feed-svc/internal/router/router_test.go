package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/external-data-feed-svc/internal/authz"
	"zoiko.io/external-data-feed-svc/internal/domain"
	"zoiko.io/external-data-feed-svc/internal/events"
	"zoiko.io/external-data-feed-svc/internal/handler"
	"zoiko.io/external-data-feed-svc/internal/idempotency"
	"zoiko.io/external-data-feed-svc/internal/router"
	"zoiko.io/external-data-feed-svc/internal/store"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

// memIdem is an in-memory idempotency.Store with PgStore's semantics.
type memIdem struct {
	mu sync.Mutex
	m  map[string]*idempotency.Record
}

func newMemIdem() *memIdem { return &memIdem{m: map[string]*idempotency.Record{}} }
func (s *memIdem) Claim(_ context.Context, t, e, k, fp string) (*idempotency.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := t + "|" + e + "|" + k
	if r, ok := s.m[id]; ok {
		if r.RequestFingerprint != fp {
			return nil, idempotency.ErrFingerprintMismatch
		}
		c := *r
		return &c, nil
	}
	s.m[id] = &idempotency.Record{RequestFingerprint: fp}
	return nil, nil
}
func (s *memIdem) Complete(_ context.Context, t, e, k string, st int, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[t+"|"+e+"|"+k]
	r.ResponseStatus, r.ResponseBody = st, append([]byte(nil), b...)
	return nil
}
func (s *memIdem) Release(_ context.Context, t, e, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[t+"|"+e+"|"+k]; ok && r.ResponseStatus == 0 {
		delete(s.m, t+"|"+e+"|"+k)
	}
	return nil
}

// policyAuthz stands in for authorization-svc: it denies exactly the actions
// in deny and grants everything else.
func policyAuthz(t *testing.T, deny ...string) *authz.Client {
	denied := map[string]bool{}
	for _, a := range deny {
		denied[a] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ActionType string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := "GRANTED"
		if denied[req.ActionType] {
			out = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}))
	t.Cleanup(srv.Close)
	return authz.NewClient(srv.URL)
}

func newRouter(t *testing.T, db pinger, az *authz.Client) http.Handler {
	h := handler.New(store.NewMemoryStore(), events.NewMockPublisher(), az, zap.NewNop())
	return router.New(router.Deps{DB: db, Idem: newMemIdem(), Handler: h, Logger: zap.NewNop()})
}

var keySeq int

func call(h http.Handler, method, path, tenant string, body any) *httptest.ResponseRecorder {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "principal-01")
	req.Header.Set("X-Request-Id", "req-1")
	req.Header.Set("X-Correlation-ID", "corr-1")
	req.Header.Set("X-Source-Channel", "api")
	req.Header.Set("X-Legal-Entity-Id", "le-1")
	keySeq++
	req.Header.Set("Idempotency-Key", fmt.Sprintf("key-%d", keySeq))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// routerMiss: chi's own 404 is text/plain "404 page not found"; handler 404s are JSON.
func routerMiss(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusMethodNotAllowed ||
		(w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "404 page not found"))
}

func createSub(t *testing.T, h http.Handler, tenant string) domain.DataFeedSubscription {
	t.Helper()
	w := call(h, "POST", "/v1/external-data-feeds/subscriptions", tenant, domain.CreateSubscriptionRequest{
		LegalEntityID: "le-1", Provider: "ECB", FeedType: domain.FeedTypeFXRate, Symbol: "EURGBP"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create subscription: %d %s", w.Code, w.Body.String())
	}
	var sub domain.DataFeedSubscription
	_ = json.Unmarshal(w.Body.Bytes(), &sub)
	return sub
}

func TestProbes_PublicAndReadyzReflectsDB(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s without headers: want 200, got %d: %s", p, w.Code, w.Body.String())
		}
	}
	down := newRouter(t, pinger{err: errors.New("db down")}, policyAuthz(t))
	w := httptest.NewRecorder()
	down.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"unavailable"`) {
		t.Fatalf("readyz with DB down: want 503 unavailable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRouteContract(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	sub := createSub(t, h, "tenant-a")
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/external-data-feeds/subscriptions", nil},
		{"GET", "/v1/external-data-feeds/subscriptions/" + sub.FeedID, nil},
		{"POST", "/v1/external-data-feeds/events/ingest", domain.IngestEventRequest{FeedID: sub.FeedID, EventType: "RATE", Payload: map[string]interface{}{"rate": 0.85}}},
		{"GET", "/v1/external-data-feeds/events", nil},
	}
	for _, rt := range routes {
		w := call(h, rt.method, rt.path, "tenant-a", rt.body)
		if routerMiss(w) || w.Code >= 500 {
			t.Errorf("%s %s: %d (%s)", rt.method, rt.path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

func TestValidation_400(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	if w := call(h, "POST", "/v1/external-data-feeds/subscriptions", "tenant-a", map[string]string{"provider": "ECB"}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing fields: want 400, got %d", w.Code)
	}
	if w := call(h, "POST", "/v1/external-data-feeds/events/ingest", "tenant-a", map[string]string{"feed_id": "x"}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing event_type: want 400, got %d", w.Code)
	}
}

func TestAuthzDeny_403(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t, handler.FEED_SUBSCRIPTION_CREATE))
	w := call(h, "POST", "/v1/external-data-feeds/subscriptions", "tenant-a", domain.CreateSubscriptionRequest{
		LegalEntityID: "le-1", Provider: "ECB", FeedType: domain.FeedTypeFXRate})
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied create: want 403, got %d: %s", w.Code, w.Body.String())
	}

	h = newRouter(t, pinger{}, policyAuthz(t, handler.FEED_EVENT_INGEST))
	sub := createSub(t, h, "tenant-a") // allowed path through the same policy stub
	w = call(h, "POST", "/v1/external-data-feeds/events/ingest", "tenant-a", domain.IngestEventRequest{FeedID: sub.FeedID, EventType: "RATE"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied ingest: want 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMissingIdentity_FailsClosed(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/external-data-feeds/subscriptions", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant: want 401, got %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/v1/external-data-feeds/subscriptions", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("write without principal: want 401, got %d", w.Code)
	}
}

func TestCrossTenant_Denied(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	sub := createSub(t, h, "tenant-a")
	if w := call(h, "GET", "/v1/external-data-feeds/subscriptions/"+sub.FeedID, "tenant-b", nil); w.Code != http.StatusNotFound {
		t.Fatalf("tenant-b read: want 404, got %d", w.Code)
	}
	// Ingesting into another tenant's feed must be refused.
	if w := call(h, "POST", "/v1/external-data-feeds/events/ingest", "tenant-b",
		domain.IngestEventRequest{FeedID: sub.FeedID, EventType: "RATE"}); w.Code != http.StatusNotFound {
		t.Fatalf("tenant-b ingest into tenant-a feed: want 404, got %d", w.Code)
	}
	if w := call(h, "GET", "/v1/external-data-feeds/subscriptions", "tenant-b", nil); !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatalf("tenant-b list must be empty: %s", w.Body.String())
	}
}

func TestIdempotentReplay_CreateSubscription(t *testing.T) {
	h := newRouter(t, pinger{}, policyAuthz(t))
	body := `{"legal_entity_id":"le-1","provider":"ECB","feed_type":"FX_RATE"}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/external-data-feeds/subscriptions", strings.NewReader(body))
		for k, v := range map[string]string{"X-Tenant-Id": "tenant-a", "X-Principal-Id": "p", "X-Request-Id": "r",
			"X-Correlation-ID": "c", "X-Source-Channel": "api", "X-Legal-Entity-Id": "le-1", "Idempotency-Key": "same-key"} {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	first, second := send(), send()
	if first.Code != http.StatusCreated || second.Header().Get("X-Idempotent-Replay") != "true" || first.Body.String() != second.Body.String() {
		t.Fatalf("replay: %d %d %q", first.Code, second.Code, second.Header().Get("X-Idempotent-Replay"))
	}
	if w := call(h, "GET", "/v1/external-data-feeds/subscriptions", "tenant-a", nil); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("replay must not create a second subscription: %s", w.Body.String())
	}
}
