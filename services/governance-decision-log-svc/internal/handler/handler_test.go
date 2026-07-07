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

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/handler"
)

// stubStore implements handler.DecisionStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	created bool
	err     error
	got     *domain.GovernanceDecision
}

func (s *stubStore) Insert(_ context.Context, d domain.GovernanceDecision) (bool, error) {
	s.got = &d
	return s.created, s.err
}

func newTestRouter(store handler.DecisionStore) http.Handler {
	r := chi.NewRouter()
	h := handler.New(store, zap.NewNop())
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
	h := newTestRouter(store)
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

// TestCreateDecision_200_IdempotentReplay verifies that a repeat POST for an
// already-stored decision_id returns 200, not 201 — proving the handler
// surfaces the store's idempotency signal rather than always reporting
// "created".
func TestCreateDecision_200_IdempotentReplay(t *testing.T) {
	store := &stubStore{created: false}
	h := newTestRouter(store)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_400_MissingField verifies that omitting any required
// field is rejected with 400 and names the missing field.
func TestCreateDecision_400_MissingField(t *testing.T) {
	body := `{"tenant_id": "tenant-1"}` // missing everything else
	h := newTestRouter(&stubStore{created: true})
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
	h := newTestRouter(&stubStore{created: true})
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
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable})
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
