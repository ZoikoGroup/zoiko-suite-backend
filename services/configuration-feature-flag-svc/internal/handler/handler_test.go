package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/handler"
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

	flag             *domain.FeatureFlag
	flagCreated      bool
	flagErr          error
	gotUpsertFlag    domain.UpsertFeatureFlagParams

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

func newTestRouter(s *stubStore, p *stubPublisher) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, zap.NewNop())
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
	req := httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertConfigEntry_InvalidJSON(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(`{not json`))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body))
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

	req := httptest.NewRequest(http.MethodGet, "/v1/config/k?environment=staging", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConfigEntry_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/config/k", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetConfigEntry_NotFound(t *testing.T) {
	store := &stubStore{findConfigErr: domain.ErrConfigEntryNotFound}
	r := newTestRouter(store, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/config/missing?environment=staging", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetConfigEntry_StoreUnavailable(t *testing.T) {
	store := &stubStore{findConfigErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/config/k?environment=staging", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GET /v1/config ───────────────────────────────────────────────────────────

func TestListConfigEntries_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{listConfigResult: nil}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/v1/config?environment=staging&tenant_id=t-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if s.gotListConfigFilter.Environment != "staging" {
		t.Errorf("expected environment=staging forwarded, got %q", s.gotListConfigFilter.Environment)
	}
	if s.gotListConfigFilter.TenantID == nil || *s.gotListConfigFilter.TenantID != "t-1" {
		t.Errorf("expected tenant_id=t-1 forwarded, got %v", s.gotListConfigFilter.TenantID)
	}
}

func TestListConfigEntries_StoreUnavailable(t *testing.T) {
	r := newTestRouter(&stubStore{listConfigErr: domain.ErrStoreUnavailable}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
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
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_NegativeRolloutPercentageRejected(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	body := `{"key":"new_ui","enabled":true,"environment":"staging","rollout_percentage":-1,"created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body))
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

	req := httptest.NewRequest(http.MethodGet, "/v1/flags/new_ui?environment=staging", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetFeatureFlag_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/flags/new_ui", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetFeatureFlag_NotFound(t *testing.T) {
	store := &stubStore{findFlagErr: domain.ErrFeatureFlagNotFound}
	r := newTestRouter(store, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/flags/missing?environment=staging", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── GET /v1/flags ────────────────────────────────────────────────────────────

func TestListFeatureFlags_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{listFlagResult: nil}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodGet, "/v1/flags", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/v1/flags", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}
