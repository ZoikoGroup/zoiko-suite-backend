package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/metric-registry-svc/internal/authz"
	"zoiko.io/metric-registry-svc/internal/domain"
	"zoiko.io/metric-registry-svc/internal/events"
)

type stubStore struct {
	versions map[string][]domain.ReportMetricDefinition // keyed by metric_code
}

func newStubStore() *stubStore {
	return &stubStore{versions: make(map[string][]domain.ReportMetricDefinition)}
}

func (s *stubStore) CreateMetricDefinition(_ context.Context, d *domain.ReportMetricDefinition) error {
	if len(s.versions[d.MetricCode]) > 0 {
		return domain.ErrConflict
	}
	d.DefinitionStatus = "ACTIVE"
	s.versions[d.MetricCode] = append(s.versions[d.MetricCode], *d)
	return nil
}

func (s *stubStore) GetActiveMetricDefinition(_ context.Context, metricCode string) (*domain.ReportMetricDefinition, error) {
	for _, v := range s.versions[metricCode] {
		if v.DefinitionStatus == "ACTIVE" {
			return &v, nil
		}
	}
	return nil, domain.ErrMetricNotFound
}

func (s *stubStore) ListMetricVersions(_ context.Context, metricCode string) ([]domain.ReportMetricDefinition, error) {
	return s.versions[metricCode], nil
}

func (s *stubStore) PublishNewVersion(_ context.Context, newVersion *domain.ReportMetricDefinition) error {
	list := s.versions[newVersion.MetricCode]
	for i := range list {
		if list[i].DefinitionStatus == "ACTIVE" {
			list[i].DefinitionStatus = "SUPERSEDED"
		}
	}
	newVersion.DefinitionStatus = "ACTIVE"
	list = append(list, *newVersion)
	s.versions[newVersion.MetricCode] = list
	return nil
}

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{ err error }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

var _ AuthzChecker = (*stubAuthz)(nil)

func newTestHandler() (*Handler, *stubStore, *stubPublisher) {
	logger, _ := zap.NewDevelopment()
	st := newStubStore()
	pub := &stubPublisher{}
	return New(st, pub, &stubAuthz{}, logger), st, pub
}

func newTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Principal-Id", "finance-metrics-owner-1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestCreateMetricDefinition_CreatesVersion1(t *testing.T) {
	h, _, pub := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode:         "MRR",
		MetricName:         "Monthly Recurring Revenue",
		FormulaDescription: "sum of active subscription base_price_amount, normalized to monthly",
		DataSources:        []string{"commercial-account-svc.commercial_subscriptions"},
		OwnerPrincipalID:   "finance-metrics-owner-1",
		EffectiveFrom:      "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var got domain.ReportMetricDefinition
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Version != 1 || got.DefinitionStatus != "ACTIVE" {
		t.Fatalf("expected version 1 ACTIVE, got %+v", got)
	}
	if got.IntelligenceDisclaimer == "" {
		t.Errorf("expected a non-empty intelligence disclaimer per doc7 REP-01")
	}
	if pub.calls != 1 {
		t.Errorf("expected report_metric.created published once, got %d", pub.calls)
	}
}

func TestCreateMetricDefinition_DuplicateCodeConflict(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	body := domain.CreateReportMetricRequest{
		MetricCode: "ARR", MetricName: "Annual Recurring Revenue", FormulaDescription: "MRR * 12",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}
	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/report-metrics", body))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/report-metrics", body))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate metric_code, got %d — %s", w.Code, w.Body.String())
	}
}

// TestPublishNewVersion_SupersedesPriorAndHistorySurvives proves the core
// versioning doctrine: publishing v2 supersedes v1 (only one ACTIVE at a
// time) but v1's row is never deleted or overwritten.
func TestPublishNewVersion_SupersedesPriorAndHistorySurvives(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode: "CHURN_RATE", MetricName: "Churn Rate", FormulaDescription: "canceled / active at period start",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))

	wPublish := httptest.NewRecorder()
	r.ServeHTTP(wPublish, buildRequest(http.MethodPost, "/v1/report-metrics/CHURN_RATE/versions", domain.PublishMetricVersionRequest{
		MetricName: "Churn Rate", FormulaDescription: "canceled / (active at period start + new in period)",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-04-01T00:00:00Z",
	}))
	if wPublish.Code != http.StatusCreated {
		t.Fatalf("expected 201 publishing v2, got %d — %s", wPublish.Code, wPublish.Body.String())
	}
	var v2 domain.ReportMetricDefinition
	_ = json.Unmarshal(wPublish.Body.Bytes(), &v2)
	if v2.Version != 2 {
		t.Fatalf("expected version 2, got %d", v2.Version)
	}

	wActive := httptest.NewRecorder()
	r.ServeHTTP(wActive, buildRequest(http.MethodGet, "/v1/report-metrics/CHURN_RATE", nil))
	var active domain.ReportMetricDefinition
	_ = json.Unmarshal(wActive.Body.Bytes(), &active)
	if active.Version != 2 {
		t.Fatalf("expected the active definition to be v2, got version %d", active.Version)
	}

	wHistory := httptest.NewRecorder()
	r.ServeHTTP(wHistory, buildRequest(http.MethodGet, "/v1/report-metrics/CHURN_RATE/versions", nil))
	var history []domain.ReportMetricDefinition
	_ = json.Unmarshal(wHistory.Body.Bytes(), &history)
	if len(history) != 2 {
		t.Fatalf("expected both v1 and v2 to survive in history, got %d entries", len(history))
	}
}

func TestPublishNewVersion_UnknownMetricCode404(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/report-metrics/DOES_NOT_EXIST/versions", domain.PublishMetricVersionRequest{
		MetricName: "x", FormulaDescription: "x", OwnerPrincipalID: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — %s", w.Code, w.Body.String())
	}
}

func TestCreateMetricDefinition_AuthorizationDenied403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode: "X", MetricName: "x", FormulaDescription: "x",
		OwnerPrincipalID: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
}
