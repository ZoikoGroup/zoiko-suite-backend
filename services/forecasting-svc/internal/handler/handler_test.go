package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/forecasting-svc/internal/authz"
	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/handler"
	"zoiko.io/forecasting-svc/internal/store"
)

// HTTP-layer tests through the real router. The pre-rewrite version drove
// store.NewMemoryStore through handler.NewHandler(store, publisher, authz,
// logger); the rewrite removed the in-memory store (Postgres is the only
// store) and the publisher (events go through the outbox). The intent of
// each old test — health check, generate→get→list→recalculate→archive,
// validation refused — is kept: routing/validation/authz here with a fake
// store, and the full lifecycle against real Postgres in handler_pg_test.go.

// policyAuthz is a policy-driven authorization-svc stand-in: an action is
// allowed only when listed; anything else is denied.
type policyAuthz struct {
	mu          sync.Mutex
	allowed     map[string]bool
	unavailable bool
	calls       []string
}

func allow(actions ...string) *policyAuthz {
	p := &policyAuthz{allowed: map[string]bool{}}
	for _, a := range actions {
		p.allowed[a] = true
	}
	return p
}

func allowAll() *policyAuthz {
	return allow(handler.ActionForecastRead, handler.ActionForecastPrepare, handler.ActionForecastApprove,
		handler.ActionForecastPublish, handler.ActionModelReleaseRegister, handler.ActionModelReleaseApprove,
		handler.ActionAssistRead, handler.ActionAssistSuggest, handler.ActionAssistDecide)
}

func (p *policyAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, principalID+"|"+legalEntityID+"|"+action)
	if p.unavailable {
		return authz.ErrAuthzServiceUnavailable
	}
	if !p.allowed[action] {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

// fakeStore implements only what these tests reach; any other method
// panics via the nil embedded interface.
type fakeStore struct {
	store.Store
	mu        sync.Mutex
	forecasts map[string]*domain.Forecast
	lastCmd   store.Command
	creates   int
	pingErr   error
}

func newFakeStore() *fakeStore { return &fakeStore{forecasts: map[string]*domain.Forecast{}} }

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) CreateForecast(_ context.Context, cmd store.Command, req domain.GenerateForecastRequest) (*domain.Forecast, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.lastCmd = cmd
	fc := &domain.Forecast{ID: "00000000-0000-0000-0000-00000000000" + string(rune('0'+f.creates)), TenantID: cmd.TenantID,
		LegalEntityID: req.LegalEntityID, ModelName: req.ModelName, Domain: req.Domain, Status: domain.ForecastCalculated,
		Version: 1, CreatedBy: cmd.ActorID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.forecasts[cmd.TenantID+"/"+fc.ID] = fc
	return fc, nil
}

func (f *fakeStore) GetForecast(_ context.Context, tenantID, id string) (*domain.Forecast, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fc, ok := f.forecasts[tenantID+"/"+id]; ok {
		return fc, nil
	}
	return nil, domain.ErrForecastNotFound
}

func (f *fakeStore) GetAssistJob(context.Context, string, string) (*domain.ForecastAssistJob, error) {
	return nil, domain.ErrAssistJobNotFound
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newRouter(st store.Store, az handler.Authorizer) http.Handler {
	return handler.NewRouter(handler.NewHandler(st, az, zap.NewNop()), pinger{})
}

// setupTestRouter is used by tenant_scope_test.go.
func setupTestRouter(t *testing.T) http.Handler {
	t.Helper()
	return newRouter(newFakeStore(), allowAll())
}

const validForecast = `{"model_name":"2026 Q3 Cash Flow","domain":"CASH_FLOW","scenario_type":"OPTIMISTIC","horizon_periods":6,
	"historical_data":[10000,12000,11500,13000,14500,16000],"historical_start_date":"2025-01-01"}`

func send(router http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test")
	withEnvelope(r)
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestHealthCheck(t *testing.T) {
	router := setupTestRouter(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal health response: %v", err)
	}
	if resp["status"] != "ok" || resp["service"] != "forecasting-svc" {
		t.Fatalf("unexpected response body: %v", resp)
	}
}

func TestReadyz_ReflectsDatabase(t *testing.T) {
	h := handler.NewHandler(newFakeStore(), allowAll(), zap.NewNop())
	for _, tc := range []struct {
		name string
		db   interface{ Ping(context.Context) error }
		want int
	}{
		{"db up", pinger{}, http.StatusOK},
		{"db down", pinger{err: errors.New("down")}, http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		handler.NewRouter(h, tc.db).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != tc.want {
			t.Fatalf("%s: readyz %d", tc.name, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	handler.NewRouter(h, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without pool: %d", rec.Code)
	}
}

func TestGenerateForecast_AuthzAllowedAndDenied(t *testing.T) {
	st := newFakeStore()
	denied := allow(handler.ActionForecastRead) // FORECAST_PREPARE not granted
	w := send(newRouter(st, denied), http.MethodPost, "/v1/forecasts/generate", validForecast, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied generate: %d %s", w.Code, w.Body.String())
	}
	if st.creates != 0 {
		t.Fatalf("denied request reached the store")
	}

	router := newRouter(st, allowAll())
	w = send(router, http.MethodPost, "/v1/forecasts/generate", validForecast, func(r *http.Request) { r.Header.Set("Idempotency-Key", "k-1") })
	if w.Code != http.StatusCreated {
		t.Fatalf("allowed generate: %d %s", w.Code, w.Body.String())
	}
	var fc domain.Forecast
	_ = json.Unmarshal(w.Body.Bytes(), &fc)
	if fc.LegalEntityID != "entity-test" || w.Header().Get("ETag") != `"v1"` || w.Header().Get("Location") != "/v1/forecasts/"+fc.ID {
		t.Fatalf("generate response: %+v %v", fc, w.Header())
	}
	if st.lastCmd.TenantID != "tenant-test" || st.lastCmd.ActorID != "principal-test" || st.lastCmd.Idempotency.Key != "k-1" {
		t.Fatalf("command: %+v", st.lastCmd)
	}
	if w := send(router, http.MethodGet, "/v1/forecasts/"+fc.ID, "", nil); w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	if w := send(router, http.MethodGet, "/v1/forecasts/"+fc.ID, "", func(r *http.Request) { r.Header.Set("X-Tenant-Id", "other-tenant") }); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get: %d", w.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, allowAll())
	fid := "/v1/forecasts/00000000-0000-0000-0000-0000000000aa"
	cases := []struct {
		name, method, path, body string
		edit                     func(*http.Request)
		want                     int
	}{
		{"missing fields", http.MethodPost, "/v1/forecasts/generate", `{"model_name":"Test"}`, nil, 422},
		{"malformed json", http.MethodPost, "/v1/forecasts/generate", `{not json`, nil, 400},
		{"empty body", http.MethodPost, "/v1/forecasts/generate", ``, nil, 400},
		{"body tenant mismatch", http.MethodPost, "/v1/forecasts/generate", `{"tenant_id":"x"}`, nil, 403},
		{"legal entity mismatch", http.MethodPost, "/v1/forecasts/generate", `{"legal_entity_id":"other"}`, nil, 403},
		{"missing principal", http.MethodGet, "/v1/forecasts", "", func(r *http.Request) { r.Header.Del("X-Principal-Id") }, 401},
		{"missing idempotency key", http.MethodPost, "/v1/forecasts/generate", validForecast, func(r *http.Request) { r.Header.Del("Idempotency-Key") }, 400},
		{"bad uuid", http.MethodGet, "/v1/forecasts/not-a-uuid", "", nil, 400},
		{"unknown forecast", http.MethodGet, fid, "", nil, 404},
		{"bad status filter", http.MethodGet, "/v1/forecasts?status=NOPE", "", nil, 400},
		{"bad limit", http.MethodGet, "/v1/forecasts?limit=500", "", nil, 400},
		{"bad job id", http.MethodGet, "/v1/forecast-assist/jobs/xyz", "", nil, 400},
		{"suggest without evidence", http.MethodPost, "/v1/forecast-assist/suggest-drivers",
			`{"domain_name":"d","model_provider":"p","model_version":"v","plan_version":"pv","planning_purpose":"pp","drivers":[{"driver_name":"x","suggested_value":1}]}`, nil, 422},
		{"start-review unknown job", http.MethodPost, "/v1/forecast-assist/jobs/faj_00000000-0000-0000-0000-000000000001:start-review", `{}`,
			func(r *http.Request) { r.Header.Set("If-Match", `"v1"`) }, 404},
		{"approve release without If-Match", http.MethodPost, "/v1/forecast-assist/model-releases/fmr_00000000-0000-0000-0000-000000000001:approve", `{"evaluation_reference":"e"}`, nil, 428},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := send(router, tc.method, tc.path, tc.body, tc.edit)
			if w.Code != tc.want {
				t.Fatalf("%s %s: want %d got %d %s", tc.method, tc.path, tc.want, w.Code, w.Body.String())
			}
		})
	}
	if st.creates != 0 {
		t.Fatalf("invalid requests must not reach the store")
	}
}

func TestAuthzUnavailable_FailsClosed503(t *testing.T) {
	az := allowAll()
	az.unavailable = true
	w := send(newRouter(newFakeStore(), az), http.MethodGet, "/v1/forecasts", "", nil)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("authz unavailable: %d %s", w.Code, w.Body.String())
	}
}
