package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/handler"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// stubStore implements handler.DecisionStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	created bool
	err     error
	got     *domain.GovernanceDecision

	findByIDResult *domain.GovernanceDecision
	findByIDErr    error

	listResult    []*domain.GovernanceDecision
	listErr       error
	listGotParams store.ListParams
}

func (s *stubStore) Insert(_ context.Context, d domain.GovernanceDecision) (bool, error) {
	s.got = &d
	return s.created, s.err
}

func (s *stubStore) FindByID(_ context.Context, decisionID string) (*domain.GovernanceDecision, error) {
	return s.findByIDResult, s.findByIDErr
}

func (s *stubStore) List(_ context.Context, params store.ListParams) ([]*domain.GovernanceDecision, error) {
	s.listGotParams = params
	return s.listResult, s.listErr
}

// stubPublisher implements handler.EventPublisher for unit testing.
// No Kafka, no network — purely in-memory, records what it was asked to publish.
type stubPublisher struct {
	err        error
	publishes  int
	gotDecision *domain.GovernanceDecision
}

func (p *stubPublisher) PublishDecisionRecorded(_ context.Context, d domain.GovernanceDecision) error {
	p.publishes++
	p.gotDecision = &d
	return p.err
}

func newTestRouter(store handler.DecisionStore, pub handler.EventPublisher) http.Handler {
	r := chi.NewRouter()
	h := handler.New(store, pub, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func validBody() string {
	return `{
		"decision_id": "dec-001",
		"tenant_id": "tenant-1",
		"legal_entity_id": "entity-1",
		"actor_id": "actor-1",
		"action_type": "PAYROLL_RELEASE",
		"outcome": "DENIED",
		"rule_basis": "policy-v3-sod",
		"correlation_id": "corr-001"
	}`
}

// TestCreateDecision_201_FirstInsert verifies the happy path: a brand new
// decision_id returns 201 with the stored decision echoed back.
func TestCreateDecision_201_FirstInsert(t *testing.T) {
	store := &stubStore{created: true}
	h := newTestRouter(store, &stubPublisher{})
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))
	req.Header.Set("X-Correlation-ID", "corr-req-001")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.GovernanceDecision
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if got.DecisionID != "dec-001" {
		t.Errorf("decision_id mismatch: got %q", got.DecisionID)
	}
	if got.DecidedAt.IsZero() {
		t.Errorf("expected decided_at to default to server time when omitted")
	}
	if rr.Header().Get("X-Correlation-ID") != "corr-req-001" {
		t.Errorf("correlation ID not echoed: got %q", rr.Header().Get("X-Correlation-ID"))
	}
}

// TestCreateDecision_201_PublishesDecisionRecorded verifies a first-time
// insert publishes governance.decision.recorded exactly once, with the
// stored decision as the payload.
func TestCreateDecision_201_PublishesDecisionRecorded(t *testing.T) {
	pub := &stubPublisher{}
	h := newTestRouter(&stubStore{created: true}, pub)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if pub.publishes != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", pub.publishes)
	}
	if pub.gotDecision == nil || pub.gotDecision.DecisionID != "dec-001" {
		t.Errorf("expected published decision dec-001, got %+v", pub.gotDecision)
	}
}

// TestCreateDecision_200_IdempotentReplay verifies that a repeat POST for an
// already-stored decision_id returns 200, not 201 — proving the handler
// surfaces the store's idempotency signal rather than always reporting
// "created".
func TestCreateDecision_200_IdempotentReplay(t *testing.T) {
	store := &stubStore{created: false}
	h := newTestRouter(store, &stubPublisher{})
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_200_IdempotentReplay_DoesNotRePublish verifies that a
// replayed decision_id (created=false) does not re-emit
// governance.decision.recorded — only the first insert is a new fact.
func TestCreateDecision_200_IdempotentReplay_DoesNotRePublish(t *testing.T) {
	pub := &stubPublisher{}
	h := newTestRouter(&stubStore{created: false}, pub)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if pub.publishes != 0 {
		t.Fatalf("expected 0 publishes on idempotent replay, got %d", pub.publishes)
	}
}

// TestCreateDecision_201_PublishFailureDoesNotFailRequest verifies that a
// publish failure is logged but does not change the HTTP response — the
// write already succeeded and event delivery is a stubbed, non-blocking
// concern.
func TestCreateDecision_201_PublishFailureDoesNotFailRequest(t *testing.T) {
	pub := &stubPublisher{err: errors.New("kafka unreachable")}
	h := newTestRouter(&stubStore{created: true}, pub)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 despite publish failure, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_400_MissingField verifies that omitting any required
// field is rejected with 400 and names the missing field.
func TestCreateDecision_400_MissingField(t *testing.T) {
	body := `{"tenant_id": "tenant-1"}` // missing everything else
	h := newTestRouter(&stubStore{created: true}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got["error"] != "missing_field" {
		t.Errorf("expected error=missing_field, got %q", got["error"])
	}
}

// TestCreateDecision_400_InvalidJSON verifies malformed JSON is rejected
// with 400, not a 500 or panic.
func TestCreateDecision_400_InvalidJSON(t *testing.T) {
	h := newTestRouter(&stubStore{created: true}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{not json`))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_503_StoreUnavailable verifies that a store error
// returns 503 — not a silently swallowed failure.
func TestCreateDecision_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got["error"] != "store_unavailable" {
		t.Errorf("expected error=store_unavailable, got %q", got["error"])
	}
}

// TestGetDecision_200_Found verifies a known decision_id returns 200 with
// the stored decision.
func TestGetDecision_200_Found(t *testing.T) {
	want := &domain.GovernanceDecision{DecisionID: "dec-001", TenantID: "tenant-1"}
	h := newTestRouter(&stubStore{findByIDResult: want}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.GovernanceDecision
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if got.DecisionID != "dec-001" {
		t.Errorf("decision_id mismatch: got %q", got.DecisionID)
	}
}

// TestGetDecision_404_NotFound verifies an unknown decision_id returns 404,
// distinct from a store failure (503).
func TestGetDecision_404_NotFound(t *testing.T) {
	h := newTestRouter(&stubStore{findByIDErr: domain.ErrDecisionNotFound}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/does-not-exist", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestGetDecision_503_StoreUnavailable verifies a non-not-found store error
// returns 503, never conflated with a legitimate 404.
func TestGetDecision_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{findByIDErr: domain.ErrStoreUnavailable}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_200_Empty verifies an empty result set serialises as an
// empty JSON array, never null.
func TestListDecisions_200_Empty(t *testing.T) {
	h := newTestRouter(&stubStore{listResult: nil}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", rr.Body.String())
	}
}

// TestListDecisions_FiltersComposeIntoListParams verifies every query
// parameter is parsed and forwarded to the store, and that they compose
// (all filters can be set simultaneously).
func TestListDecisions_FiltersComposeIntoListParams(t *testing.T) {
	s := &stubStore{listResult: []*domain.GovernanceDecision{{DecisionID: "dec-001"}}}
	h := newTestRouter(s, &stubPublisher{})

	q := url.Values{
		"actor":      {"actor-1"},
		"entity":     {"entity-1"},
		"action":     {"PAYROLL_RELEASE"},
		"rule_basis": {"policy-v3-sod"},
		"from":       {"2024-01-01T00:00:00Z"},
		"to":         {"2024-12-31T23:59:59Z"},
		"limit":      {"10"},
		"offset":     {"5"},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions?"+q.Encode(), nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	p := s.listGotParams
	if p.ActorID != "actor-1" || p.LegalEntityID != "entity-1" || p.ActionType != "PAYROLL_RELEASE" || p.RuleBasis != "policy-v3-sod" {
		t.Errorf("filters not forwarded correctly: %+v", p)
	}
	if p.From.IsZero() || p.To.IsZero() {
		t.Errorf("expected from/to to be parsed, got %+v", p)
	}
	if p.Limit != 10 || p.Offset != 5 {
		t.Errorf("expected limit=10 offset=5, got limit=%d offset=%d", p.Limit, p.Offset)
	}
}

// TestListDecisions_400_InvalidFrom verifies a malformed from timestamp is
// rejected with 400 rather than silently ignored or causing a 500.
func TestListDecisions_400_InvalidFrom(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions?from=not-a-timestamp", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_400_InvalidTo verifies a malformed to timestamp is
// rejected with 400.
func TestListDecisions_400_InvalidTo(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions?to=not-a-timestamp", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_503_StoreUnavailable verifies a store failure returns
// 503, not a silently empty list.
func TestListDecisions_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{listErr: domain.ErrStoreUnavailable}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
}
