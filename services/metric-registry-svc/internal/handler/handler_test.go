package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/metric-registry-svc/internal/authz"
	"zoiko.io/metric-registry-svc/internal/domain"
	"zoiko.io/metric-registry-svc/internal/events"
	"zoiko.io/metric-registry-svc/internal/outbox"
)

type stubStore struct {
	versions     map[string][]domain.ReportMetricDefinition // keyed by metric_code
	outboxEvents []outbox.Event
}

func newStubStore() *stubStore {
	return &stubStore{versions: make(map[string][]domain.ReportMetricDefinition)}
}

func (s *stubStore) CreateMetricDefinition(_ context.Context, d *domain.ReportMetricDefinition, outboxEvt outbox.Event) error {
	if len(s.versions[d.MetricCode]) > 0 {
		if existing, _ := s.FindByIdempotencyKey(context.Background(), d.MetricCode, d.IdempotencyKey); existing != nil {
			*d = *existing
			return nil
		}
		return domain.ErrConflict
	}
	d.DefinitionStatus = "ACTIVE"
	s.versions[d.MetricCode] = append(s.versions[d.MetricCode], *d)
	s.outboxEvents = append(s.outboxEvents, outboxEvt)
	return nil
}

func (s *stubStore) FindByIdempotencyKey(_ context.Context, metricCode, idempotencyKey string) (*domain.ReportMetricDefinition, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	for _, v := range s.versions[metricCode] {
		if v.IdempotencyKey == idempotencyKey {
			found := v
			return &found, nil
		}
	}
	return nil, nil
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

func (s *stubStore) ListActiveMetricDefinitions(_ context.Context, limit, offset int) ([]domain.ReportMetricDefinition, error) {
	var codes []string
	for code := range s.versions {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	out := make([]domain.ReportMetricDefinition, 0)
	for _, code := range codes {
		for _, v := range s.versions[code] {
			if v.DefinitionStatus == "ACTIVE" {
				out = append(out, v)
			}
		}
	}
	if offset >= len(out) {
		return []domain.ReportMetricDefinition{}, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

func (s *stubStore) PublishNewVersion(_ context.Context, newVersion *domain.ReportMetricDefinition, outboxEvt outbox.Event) error {
	if existing, _ := s.FindByIdempotencyKey(context.Background(), newVersion.MetricCode, newVersion.IdempotencyKey); existing != nil {
		*newVersion = *existing
		return nil
	}
	list := s.versions[newVersion.MetricCode]
	for i := range list {
		if list[i].DefinitionStatus == "ACTIVE" {
			list[i].DefinitionStatus = "SUPERSEDED"
		}
	}
	newVersion.DefinitionStatus = "ACTIVE"
	list = append(list, *newVersion)
	s.versions[newVersion.MetricCode] = list
	s.outboxEvents = append(s.outboxEvents, outboxEvt)
	return nil
}

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
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
	h, st, _ := newTestHandler()
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
	// Delivered via the transactional outbox (st.outboxEvents), not a direct
	// publisher call — see handler.go's CreateMetricDefinition, which used
	// to call h.publisher.Publish directly with the error discarded.
	if len(st.outboxEvents) != 1 {
		t.Errorf("expected 1 outbox event for report_metric.created, got %d", len(st.outboxEvents))
	} else if st.outboxEvents[0].EventType != "report_metric.created" {
		t.Errorf("expected event type report_metric.created, got %s", st.outboxEvents[0].EventType)
	}
}

// TestCreateMetricDefinition_IdempotentReplay_ReturnsOriginal is the
// regression test for a gap found this pass: INV-08 requires Idempotency-Key
// to protect against duplicate/replay, but the header was only ever checked
// for presence by the envelope middleware, never actually used for
// deduplication. Before this fix, retrying the same logical create request
// with the same Idempotency-Key but relying on metric_code's own uniqueness
// returned a 409 Conflict — the wrong outcome for a genuine replay.
func TestCreateMetricDefinition_IdempotentReplay_ReturnsOriginal(t *testing.T) {
	h, st, _ := newTestHandler()
	r := newTestRouter(h)

	req := func() *http.Request {
		rq := buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
			MetricCode: "IDEMPOTENT_CREATE", MetricName: "x", FormulaDescription: "x",
			OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
		})
		rq.Header.Set("Idempotency-Key", "replay-key-create-1")
		return rq
	}

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req())
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d — %s", w1.Code, w1.Body.String())
	}
	var first domain.ReportMetricDefinition
	_ = json.Unmarshal(w1.Body.Bytes(), &first)

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req())
	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 201 (replay of original), got %d — %s", w2.Code, w2.Body.String())
	}
	var second domain.ReportMetricDefinition
	_ = json.Unmarshal(w2.Body.Bytes(), &second)
	if second.MetricDefinitionID != first.MetricDefinitionID {
		t.Fatalf("replay must return the ORIGINAL row, got a different metric_definition_id: %s vs %s", first.MetricDefinitionID, second.MetricDefinitionID)
	}
	if len(st.versions["IDEMPOTENT_CREATE"]) != 1 {
		t.Fatalf("replay must not create a second row, got %d rows", len(st.versions["IDEMPOTENT_CREATE"]))
	}
	if len(st.outboxEvents) != 1 {
		t.Fatalf("replay must not emit a second outbox event, got %d", len(st.outboxEvents))
	}
}

// TestPublishNewVersion_IdempotentReplay_DoesNotDoubleSupersede is the
// regression test for the more serious half of the same gap: unlike create,
// publish's supersede-then-insert is not self-protecting against a replay —
// a retried call that reached the store would supersede whatever a
// legitimate LATER publish had since made ACTIVE. This proves a replay
// returns the original version unconditionally and never touches the table.
func TestPublishNewVersion_IdempotentReplay_DoesNotDoubleSupersede(t *testing.T) {
	h, st, _ := newTestHandler()
	r := newTestRouter(h)

	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode: "IDEMPOTENT_PUBLISH", MetricName: "x", FormulaDescription: "x",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))

	publishReq := func() *http.Request {
		rq := buildRequest(http.MethodPost, "/v1/report-metrics/IDEMPOTENT_PUBLISH/versions", domain.PublishMetricVersionRequest{
			MetricName: "x", FormulaDescription: "y",
			OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-04-01T00:00:00Z",
		})
		rq.Header.Set("Idempotency-Key", "replay-key-publish-1")
		return rq
	}

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, publishReq())
	var v2 domain.ReportMetricDefinition
	_ = json.Unmarshal(w1.Body.Bytes(), &v2)
	if v2.Version != 2 {
		t.Fatalf("expected version 2 on first publish, got %d", v2.Version)
	}

	// A legitimate, later publish with a DIFFERENT idempotency key — this
	// must remain ACTIVE even after the stale replay below is (re)sent.
	wLater := httptest.NewRecorder()
	laterReq := buildRequest(http.MethodPost, "/v1/report-metrics/IDEMPOTENT_PUBLISH/versions", domain.PublishMetricVersionRequest{
		MetricName: "x", FormulaDescription: "z",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-07-01T00:00:00Z",
	})
	laterReq.Header.Set("Idempotency-Key", "later-key-publish-2")
	r.ServeHTTP(wLater, laterReq)
	var v3 domain.ReportMetricDefinition
	_ = json.Unmarshal(wLater.Body.Bytes(), &v3)
	if v3.Version != 3 {
		t.Fatalf("expected version 3 for the later publish, got %d", v3.Version)
	}

	// Now replay the FIRST publish's request again — it must return v2
	// unconditionally and must NOT supersede v3, which is the bug this
	// check prevents.
	wReplay := httptest.NewRecorder()
	r.ServeHTTP(wReplay, publishReq())
	var replayed domain.ReportMetricDefinition
	_ = json.Unmarshal(wReplay.Body.Bytes(), &replayed)
	if replayed.Version != 2 {
		t.Fatalf("replay must return the ORIGINAL v2, got version %d", replayed.Version)
	}

	current, err := st.GetActiveMetricDefinition(context.Background(), "IDEMPOTENT_PUBLISH")
	if err != nil {
		t.Fatalf("GetActiveMetricDefinition: %v", err)
	}
	if current.Version != 3 {
		t.Fatalf("replaying the stale v2 publish must not disturb v3's ACTIVE status, but current ACTIVE is version %d", current.Version)
	}
	if len(st.versions["IDEMPOTENT_PUBLISH"]) != 3 {
		t.Fatalf("expected exactly 3 rows (v1, v2, v3), got %d", len(st.versions["IDEMPOTENT_PUBLISH"]))
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

// requestWithoutIdentity is buildRequest minus the X-Principal-Id header,
// for the two regression tests below.
func requestWithoutIdentity(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestGetActiveMetricDefinition_RequiresPrincipal and
// TestListMetricVersions_RequiresPrincipal are the regression tests for a
// gap found this pass: these two reads had no identity requirement at all,
// unlike every other read in this handler and unlike the same platform-wide
// catalog pattern this service's own code already follows elsewhere
// (capability-registry-svc, schema-registry-svc) — any caller on the
// internal network could enumerate the full metric definition catalog,
// including owner_principal_id and formula_description, anonymously.
func TestGetActiveMetricDefinition_RequiresPrincipal(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, requestWithoutIdentity(http.MethodGet, "/v1/report-metrics/MRR"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d — %s", w.Code, w.Body.String())
	}
}

func TestListMetricVersions_RequiresPrincipal(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, requestWithoutIdentity(http.MethodGet, "/v1/report-metrics/MRR/versions"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d — %s", w.Code, w.Body.String())
	}
}

// TestListActiveMetricDefinitions_ReturnsCatalog is the regression test for
// MET-02: GET /v1/report-metrics (no metric_code) used to have no handler
// at all, so chi answered 405 Method Not Allowed — the only method
// registered on that exact path was POST — and the frontend metric catalog
// rendered an empty table.
func TestListActiveMetricDefinitions_ReturnsCatalog(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode: "ARR", MetricName: "Annual Recurring Revenue", FormulaDescription: "MRR * 12",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode: "MRR", MetricName: "Monthly Recurring Revenue", FormulaDescription: "sum of active subscriptions",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/report-metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (was this still 405 Method Not Allowed?) — %s", w.Code, w.Body.String())
	}
	var catalog []domain.ReportMetricDefinition
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog response: %v", err)
	}
	if len(catalog) != 2 {
		t.Fatalf("expected both metric codes in the catalog, got %d entries: %+v", len(catalog), catalog)
	}
}

func TestListActiveMetricDefinitions_RequiresPrincipal(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, requestWithoutIdentity(http.MethodGet, "/v1/report-metrics"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d — %s", w.Code, w.Body.String())
	}
}

func TestListActiveMetricDefinitions_InvalidLimitRejected(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/report-metrics?limit=0", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an out-of-range limit, got %d — %s", w.Code, w.Body.String())
	}
}

// TestPublishNewVersion_OutboxEventCreated proves report_metric.version_published
// is delivered via the outbox, mirroring TestCreateMetricDefinition_CreatesVersion1.
func TestPublishNewVersion_OutboxEventCreated(t *testing.T) {
	h, st, _ := newTestHandler()
	r := newTestRouter(h)

	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/report-metrics", domain.CreateReportMetricRequest{
		MetricCode: "NRR", MetricName: "Net Revenue Retention", FormulaDescription: "x",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/report-metrics/NRR/versions", domain.PublishMetricVersionRequest{
		MetricName: "Net Revenue Retention", FormulaDescription: "y",
		OwnerPrincipalID: "finance-metrics-owner-1", EffectiveFrom: "2026-04-01T00:00:00Z",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	if len(st.outboxEvents) != 2 { // report_metric.created + report_metric.version_published
		t.Fatalf("expected 2 outbox events, got %d", len(st.outboxEvents))
	}
	if st.outboxEvents[1].EventType != "report_metric.version_published" {
		t.Fatalf("expected the second outbox event to be report_metric.version_published, got %s", st.outboxEvents[1].EventType)
	}
}
