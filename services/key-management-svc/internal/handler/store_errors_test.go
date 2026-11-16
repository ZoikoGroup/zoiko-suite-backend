package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
	"zoiko.io/key-management-svc/internal/testpg"
)

// failingStore is a key store whose every call fails the way a database
// outage does: with an error that is NOT store.ErrKeyNotFound.
type failingStore struct{ err error }

func (f failingStore) CreateKey(context.Context, string, *domain.CustomerKey) error { return f.err }
func (f failingStore) GetKeyByID(context.Context, string, string) (*domain.CustomerKey, error) {
	return nil, f.err
}
func (f failingStore) ListKeys(context.Context, string, string) ([]domain.CustomerKey, error) {
	return nil, f.err
}
func (f failingStore) RotateKey(context.Context, string, string) (*domain.CustomerKey, error) {
	return nil, f.err
}
func (f failingStore) DisableKey(context.Context, string, string) error { return f.err }

// storeErrorRoutes is every key route whose store errors used to be flattened: the
// four Phase-4 handlers, each with a request that reaches its store call.
func storeErrorRoutes(id string) map[string]*http.Request {
	return map[string]*http.Request{
		"GetKey":     httptest.NewRequest(http.MethodGet, "/v1/keys/"+id, nil),
		"ListKeys":   httptest.NewRequest(http.MethodGet, "/v1/keys/?legal_entity_id=le-1", nil),
		"RotateKey":  httptest.NewRequest(http.MethodPost, "/v1/keys/"+id+"/rotate", nil),
		"DisableKey": httptest.NewRequest(http.MethodPost, "/v1/keys/"+id+"/disable", nil),
	}
}

func serveKeyRoute(router http.Handler, req *http.Request, tenant string) *httptest.ResponseRecorder {
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("X-Principal-Id", "principal-test-01")
	withEnvelope(req)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p.Code
}

// A store failure is 503 and retryable on all four routes — never 404, and
// never a 200 with an empty list.
func TestKeyRoutes_StoreFailureIs503(t *testing.T) {
	router := handler.NewRouter(handler.New(failingStore{err: errors.New("dial tcp: connection refused")},
		siem.New("", "key-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop()))
	for name, req := range storeErrorRoutes(uuid.NewString()) {
		w := serveKeyRoute(router, req, uuid.NewString())
		if w.Code != http.StatusServiceUnavailable || problemCode(t, w) != "service_unavailable" ||
			!strings.Contains(w.Body.String(), `"retryable":true`) {
			t.Errorf("%s with the store down: %d %s; want 503 service_unavailable, retryable", name, w.Code, w.Body.String())
		}
	}
}

// The other side of the distinction: a genuine miss is still 404, and a
// genuinely empty list is still 200 with no keys.
func TestKeyRoutes_NotFoundAndEmptyAreStillAnswered(t *testing.T) {
	router := handler.NewRouter(handler.New(store.NewMemoryStore(),
		siem.New("", "key-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop()))
	tenant := uuid.NewString()
	for name, req := range storeErrorRoutes(uuid.NewString()) {
		w := serveKeyRoute(router, req, tenant)
		want := http.StatusNotFound
		if name == "ListKeys" {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Errorf("%s on an empty store: %d %s; want %d", name, w.Code, w.Body.String(), want)
		}
		if name == "ListKeys" && !strings.Contains(w.Body.String(), `"count":0`) {
			t.Errorf("empty list: %s", w.Body.String())
		}
	}
}

// The same distinction against a real Postgres outage rather than a stub: a
// PgStore whose pool has been closed fails every call, and every route
// answers 503. With the pool open, the same routes answer 404 / 200.
func TestKeyRoutes_RealDatabaseOutageIs503(t *testing.T) {
	pools := testpg.Open(t, "kms_handler_test")
	pg := store.NewPgStore(pools.App)
	router := handler.NewRouter(handler.New(pg, siem.New("", "key-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop()))
	tenant, id := uuid.NewString(), uuid.NewString()

	for name, req := range storeErrorRoutes(id) {
		if w := serveKeyRoute(router, req, tenant); w.Code == http.StatusServiceUnavailable {
			t.Fatalf("%s with the database up: 503 %s", name, w.Body.String())
		}
	}
	pools.App.Close() // the outage
	for name, req := range storeErrorRoutes(id) {
		if w := serveKeyRoute(router, req, tenant); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with the database down: %d %s; want 503", name, w.Code, w.Body.String())
		}
	}
}

// A tenant that is not a UUID can own no row: 400, not 404 and not 503.
func TestKeyRoutes_NonUUIDTenantIs400OnPostgres(t *testing.T) {
	pg := store.NewPgStore(testpg.Open(t, "kms_handler_test").App)
	router := handler.NewRouter(handler.New(pg, siem.New("", "key-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop()))
	for name, req := range storeErrorRoutes(uuid.NewString()) {
		if w := serveKeyRoute(router, req, "t1"); w.Code != http.StatusBadRequest {
			t.Errorf("%s as tenant t1: %d %s; want 400", name, w.Code, w.Body.String())
		}
	}
}
