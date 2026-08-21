package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/authz"
	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/handler"
	svcmiddleware "zoiko.io/configuration-feature-flag-svc/internal/middleware"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── stub store ────────────────────────────────────────────────────────────────

// stubStore implements handler.ConfigStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	configEntry        *domain.ConfigEntry
	configEntryCreated bool
	configEntryErr     error
	gotUpsertConfig    domain.UpsertConfigEntryParams

	findConfigResult *domain.ConfigEntry
	findConfigErr    error

	listConfigResult    []*domain.ConfigEntry
	listConfigErr       error
	gotListConfigFilter store.ListFilter

	flag          *domain.FeatureFlag
	flagCreated   bool
	flagErr       error
	gotUpsertFlag domain.UpsertFeatureFlagParams

	findFlagResult *domain.FeatureFlag
	findFlagErr    error

	listFlagResult    []*domain.FeatureFlag
	listFlagErr       error
	gotListFlagFilter store.ListFilter
}

func (s *stubStore) UpsertConfigEntry(_ context.Context, params domain.UpsertConfigEntryParams) (*domain.ConfigEntry, bool, error) {
	s.gotUpsertConfig = params
	return s.configEntry, s.configEntryCreated, s.configEntryErr
}

func (s *stubStore) FindCurrentConfigEntry(_ context.Context, _, _ string, _ *string) (*domain.ConfigEntry, error) {
	return s.findConfigResult, s.findConfigErr
}

func (s *stubStore) ListCurrentConfigEntries(_ context.Context, filter store.ListFilter) ([]*domain.ConfigEntry, error) {
	s.gotListConfigFilter = filter
	return s.listConfigResult, s.listConfigErr
}

func (s *stubStore) UpsertFeatureFlag(_ context.Context, params domain.UpsertFeatureFlagParams) (*domain.FeatureFlag, bool, error) {
	s.gotUpsertFlag = params
	return s.flag, s.flagCreated, s.flagErr
}

func (s *stubStore) FindCurrentFeatureFlag(_ context.Context, _, _ string, _ *string) (*domain.FeatureFlag, error) {
	return s.findFlagResult, s.findFlagErr
}

func (s *stubStore) ListCurrentFeatureFlags(_ context.Context, filter store.ListFilter) ([]*domain.FeatureFlag, error) {
	s.gotListFlagFilter = filter
	return s.listFlagResult, s.listFlagErr
}

// ── stub publisher ───────────────────────────────────────────────────────────

// stubPublisher implements handler.EventPublisher for unit testing.
type stubPublisher struct {
	err              error
	configCalls      int
	featureFlagCalls int
}

func (p *stubPublisher) PublishConfigUpdated(_ context.Context, _ domain.ConfigEntry, _ string) error {
	p.configCalls++
	return p.err
}

func (p *stubPublisher) PublishFeatureFlagUpdated(_ context.Context, _ domain.FeatureFlag, _ string) error {
	p.featureFlagCalls++
	return p.err
}

// newTestRouter mounts TenantContext, which cmd/server/main.go mounts in front
// of these same routes. This service had no tenant middleware at all until the
// scope was closed, so nothing here ever exercised a scoped request.
func newTestRouter(s *stubStore, p *stubPublisher) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, testAuthz(), testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// ── POST /v1/config ──────────────────────────────────────────────────────────

func TestUpsertConfigEntry_Created(t *testing.T) {
	store := &stubStore{
		configEntry: &domain.ConfigEntry{
			ConfigID:    "cfg-1",
			Key:         "payroll.batch_size",
			Value:       []byte(`100`),
			Environment: "staging",
		},
		configEntryCreated: true,
	}
	pub := &stubPublisher{}
	r := newTestRouter(store, pub)

	body := `{"key":"payroll.batch_size","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if pub.configCalls != 1 {
		t.Errorf("expected config.updated published once, got %d", pub.configCalls)
	}
	var got domain.ConfigEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Key != "payroll.batch_size" {
		t.Errorf("expected key payroll.batch_size, got %s", got.Key)
	}
}

func TestUpsertConfigEntry_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{
		configEntry:        &domain.ConfigEntry{ConfigID: "cfg-1", Key: "k"},
		configEntryCreated: false,
	}
	pub := &stubPublisher{}
	r := newTestRouter(store, pub)

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent no-op, got %d: %s", w.Code, w.Body.String())
	}
	if pub.configCalls != 0 {
		t.Errorf("expected config.updated NOT published on no-op, got %d calls", pub.configCalls)
	}
}

func TestUpsertConfigEntry_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	body := `{"key":"k"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertConfigEntry_InvalidJSON(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(`{not json`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertConfigEntry_StoreUnavailable(t *testing.T) {
	store := &stubStore{configEntryErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store, &stubPublisher{})

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestUpsertConfigEntry_PublishFailureDoesNotFailRequest(t *testing.T) {
	store := &stubStore{configEntry: &domain.ConfigEntry{ConfigID: "cfg-1"}, configEntryCreated: true}
	pub := &stubPublisher{err: context.DeadlineExceeded}
	r := newTestRouter(store, pub)

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 despite publish failure, got %d: %s", w.Code, w.Body.String())
	}
}

// ── GET /v1/config/{key} ─────────────────────────────────────────────────────

func TestGetConfigEntry_Found(t *testing.T) {
	store := &stubStore{findConfigResult: &domain.ConfigEntry{ConfigID: "cfg-1", Key: "k", Value: []byte(`100`)}}
	r := newTestRouter(store, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/k?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConfigEntry_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/k", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetConfigEntry_NotFound(t *testing.T) {
	store := &stubStore{findConfigErr: domain.ErrConfigEntryNotFound}
	r := newTestRouter(store, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/missing?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetConfigEntry_StoreUnavailable(t *testing.T) {
	store := &stubStore{findConfigErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/k?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GET /v1/config ───────────────────────────────────────────────────────────

func TestListConfigEntries_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{listConfigResult: nil}, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListConfigEntries_FiltersForwarded(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config?environment=staging&tenant_id="+testTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if s.gotListConfigFilter.Environment != "staging" {
		t.Errorf("expected environment=staging forwarded, got %q", s.gotListConfigFilter.Environment)
	}
	if s.gotListConfigFilter.TenantID == nil || *s.gotListConfigFilter.TenantID != testTenant {
		t.Errorf("expected the verified tenant %s forwarded, got %v", testTenant, s.gotListConfigFilter.TenantID)
	}
}

func TestListConfigEntries_StoreUnavailable(t *testing.T) {
	r := newTestRouter(&stubStore{listConfigErr: domain.ErrStoreUnavailable}, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── POST /v1/flags ───────────────────────────────────────────────────────────

func TestUpsertFeatureFlag_Created(t *testing.T) {
	store := &stubStore{
		flag:        &domain.FeatureFlag{FlagID: "flag-1", Key: "new_ui", Enabled: true, RolloutPercentage: 100},
		flagCreated: true,
	}
	pub := &stubPublisher{}
	r := newTestRouter(store, pub)

	body := `{"key":"new_ui","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if pub.featureFlagCalls != 1 {
		t.Errorf("expected feature_flag.updated published once, got %d", pub.featureFlagCalls)
	}
	if store.gotUpsertFlag.RolloutPercentage != 100 {
		t.Errorf("expected rollout_percentage to default to 100, got %d", store.gotUpsertFlag.RolloutPercentage)
	}
}

func TestUpsertFeatureFlag_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{flag: &domain.FeatureFlag{FlagID: "flag-1"}, flagCreated: false}
	pub := &stubPublisher{}
	r := newTestRouter(store, pub)

	body := `{"key":"new_ui","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pub.featureFlagCalls != 0 {
		t.Errorf("expected feature_flag.updated NOT published on no-op, got %d calls", pub.featureFlagCalls)
	}
}

func TestUpsertFeatureFlag_MissingEnabled(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	body := `{"key":"new_ui","environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertFeatureFlag_ExplicitFalseIsNotMissing(t *testing.T) {
	store := &stubStore{flag: &domain.FeatureFlag{FlagID: "flag-1"}, flagCreated: true}
	r := newTestRouter(store, &stubPublisher{})

	body := `{"key":"new_ui","enabled":false,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for explicit enabled=false, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotUpsertFlag.Enabled != false {
		t.Errorf("expected Enabled=false forwarded, got %v", store.gotUpsertFlag.Enabled)
	}
}

func TestUpsertFeatureFlag_RolloutPercentageOutOfRange(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	body := `{"key":"new_ui","enabled":true,"environment":"staging","rollout_percentage":150,"created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_NegativeRolloutPercentageRejected(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	body := `{"key":"new_ui","enabled":true,"environment":"staging","rollout_percentage":-1,"created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_StoreUnavailable(t *testing.T) {
	store := &stubStore{flagErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store, &stubPublisher{})

	body := `{"key":"new_ui","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GET /v1/flags/{key} ──────────────────────────────────────────────────────

func TestGetFeatureFlag_Found(t *testing.T) {
	store := &stubStore{findFlagResult: &domain.FeatureFlag{FlagID: "flag-1", Key: "new_ui"}}
	r := newTestRouter(store, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags/new_ui?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetFeatureFlag_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags/new_ui", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetFeatureFlag_NotFound(t *testing.T) {
	store := &stubStore{findFlagErr: domain.ErrFeatureFlagNotFound}
	r := newTestRouter(store, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags/missing?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── GET /v1/flags ────────────────────────────────────────────────────────────

func TestListFeatureFlags_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{listFlagResult: nil}, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListFeatureFlags_StoreUnavailable(t *testing.T) {
	r := newTestRouter(&stubStore{listFlagErr: domain.ErrStoreUnavailable}, &stubPublisher{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── authorization test scaffolding ───────────────────────────────────────────

// testAuthzScopeID stands in for config.AuthZPlatformScopeID.
const testAuthzScopeID = "00000000-0000-0000-0000-0000000000f3"

// testPrincipal is what the gateway ForwardAuth middleware sets in
// X-Principal-Id after verifying the caller identity envelope.
const testPrincipal = "principal-test-admin"

// testTenant is the caller's verified tenant scope; tenant_id is a uuid column.
const testTenant = "11111111-1111-1111-1111-111111111111"

// otherTenant is a tenant the caller has no scope in.
const otherTenant = "22222222-2222-2222-2222-222222222222"

// stubAuthz records what it was asked and answers with err.
type stubAuthz struct {
	err error

	calls      int
	principal  string
	scope      string
	actionType string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.calls++
	a.principal, a.scope, a.actionType = principalID, legalEntityID, actionType
	return a.err
}

// testAuthz is the permit-all default used by every pre-existing test.
func testAuthz() *stubAuthz { return &stubAuthz{} }

// authed stamps the gateway-verified principal header onto a request.
// Every mutating route now requires it.
func authed(req *http.Request) *http.Request {
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("X-Tenant-Id", testTenant)
	return req
}

// scoped stamps only the tenant scope, for reads that need no principal.
func scoped(req *http.Request) *http.Request {
	req.Header.Set("X-Tenant-Id", testTenant)
	return req
}

// ── authorization contract ───────────────────────────────────────────────────

// gatedRoutes is every route that mutates state, so a route added later
// cannot quietly skip the gate.
var gatedRoutes = []struct {
	name string
	path string
	body string
}{
	{name: "upsert config", path: "/v1/config", body: `{"config_key":"k","config_value":"v","environment":"production","updated_by_principal_id":"a"}`},
	{name: "upsert flag", path: "/v1/flags", body: `{"flag_key":"f","environment":"production","enabled":true,"updated_by_principal_id":"a"}`},
}

// TestGatedRoutes_401_WithoutPrincipal — this service shipped with no gate of
// any kind, so anything able to reach the port could write to it.
func TestGatedRoutes_401_WithoutPrincipal(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			pub := &stubPublisher{}
			az := &stubAuthz{}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, pub, az, testAuthzScopeID, zap.NewNop()))

			// Deliberately NOT wrapped in authed().
			req := httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a principal, got %d: %s", w.Code, w.Body.String())
			}
			if az.calls != 0 {
				t.Error("authorization was consulted before the caller was even identified")
			}
		})
	}
}

// TestGatedRoutes_403_Denied — a denial must stop the write.
func TestGatedRoutes_403_Denied(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			pub := &stubPublisher{}
			az := &stubAuthz{err: authz.ErrDenied}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, pub, az, testAuthzScopeID, zap.NewNop()))

			req := authed(httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
			if az.principal != testPrincipal {
				t.Errorf("authz saw principal %q, want %q", az.principal, testPrincipal)
			}
			if az.scope == "" {
				t.Error("an empty legal_entity_id would be rejected by authorization-svc with 400")
			}
		})
	}
}

// TestGatedRoutes_503_AuthzUnavailableFailsClosed — an unreachable
// authorization service must block the mutation, not wave it through.
func TestGatedRoutes_503_AuthzUnavailableFailsClosed(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			pub := &stubPublisher{}
			az := &stubAuthz{err: authz.ErrUnavailable}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, pub, az, testAuthzScopeID, zap.NewNop()))

			req := authed(httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────
//
// This service read no tenant header at all before these tests existed: a
// tenant_id in a body chose whose configuration to overwrite, and an ABSENT
// ?tenant_id= on a list route was documented as "entries across all tenants".

func TestListConfigEntries_NoTenantScope_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
	}
}

// The list used to be unfiltered when ?tenant_id= was omitted. It is now the
// caller's own tenant plus the global defaults that apply to it.
func TestListConfigEntries_ScopedToVerifiedTenantPlusGlobal(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s, &stubPublisher{})
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotListConfigFilter.TenantID == nil || *s.gotListConfigFilter.TenantID != testTenant {
		t.Fatalf("expected the list scoped to the verified tenant %s, got %v", testTenant, s.gotListConfigFilter.TenantID)
	}
	if !s.gotListConfigFilter.IncludeGlobal {
		t.Fatal("expected global defaults included — they apply to this tenant too")
	}
}

func TestListConfigEntries_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config?tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's configuration, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListFeatureFlags_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags?tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's flags, got %d: %s", w.Code, w.Body.String())
	}
}

// Configuration is what other services read to decide how to behave, so a
// cross-tenant write here changes another tenant's runtime behaviour.
func TestUpsertConfigEntry_ForeignTenantBody_Refused(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s, &stubPublisher{})
	body := `{"key":"k","value":"v","environment":"staging","tenant_id":"` + otherTenant + `","created_by_principal_id":"` + testPrincipal + `"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 writing into another tenant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_ForeignTenantBody_Refused(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s, &stubPublisher{})
	body := `{"key":"new_ui","enabled":true,"environment":"staging","tenant_id":"` + otherTenant + `","created_by_principal_id":"` + testPrincipal + `"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 flipping another tenant's flag, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConfigEntry_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})
	req := scoped(httptest.NewRequest(http.MethodGet,
		"/v1/config/k?environment=staging&tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 reading another tenant's config entry, got %d: %s", w.Code, w.Body.String())
	}
}
