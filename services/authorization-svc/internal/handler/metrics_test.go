package handler_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

type recordedDecision struct{ decision, outcome, layer, action string }

type stubMetrics struct {
	mu          sync.Mutex
	decisions   []recordedDecision
	evaluations int
	failures    int
}

func (s *stubMetrics) ObserveDecision(decision, outcome, layer, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, recordedDecision{decision, outcome, layer, action})
}
func (s *stubMetrics) ObserveEvaluation(time.Duration) { s.mu.Lock(); s.evaluations++; s.mu.Unlock() }
func (s *stubMetrics) DecisionRecordFailed()           { s.mu.Lock(); s.failures++; s.mu.Unlock() }

func routerWithMetrics(store *stubStore, m handler.DecisionMetrics) chi.Router {
	h := handler.New(store, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop())
	h.SetMetrics(m)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

func TestMetrics_RecordedDecisionsAreCountedWithTheirLayer(t *testing.T) {
	m := &stubMetrics{}
	r := routerWithMetrics(&stubStore{rbacActions: []string{"PAYMENT_APPROVE"}, rbacBasis: "rbac:role=FINANCE_APPROVER"}, m)

	for _, action := range []string{"PAYMENT_APPROVE", "PAYMENT_RELEASE"} {
		body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"` + action + `"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
		req.Header.Set("X-Tenant-Id", testTenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code %d %s", action, w.Code, w.Body.String())
		}
	}
	want := []recordedDecision{
		{"PERMIT", "GRANTED", "rbac", "PAYMENT_APPROVE"},
		{"DENY", "DENIED", "no_grant", "PAYMENT_RELEASE"},
	}
	if len(m.decisions) != len(want) {
		t.Fatalf("decisions = %+v, want %+v", m.decisions, want)
	}
	for i := range want {
		if m.decisions[i] != want[i] {
			t.Errorf("decision %d = %+v, want %+v", i, m.decisions[i], want[i])
		}
	}
	if m.evaluations != 2 || m.failures != 0 {
		t.Fatalf("evaluations=%d failures=%d, want 2 / 0", m.evaluations, m.failures)
	}
}

// A decision whose artefact cannot be written is not counted as a decision —
// the PEP got a 503 — but as a record failure.
func TestMetrics_UnrecordedDecisionCountsAsFailure(t *testing.T) {
	m := &stubMetrics{}
	r := routerWithMetrics(&stubStore{rbacActions: []string{"PAYMENT_APPROVE"}, rbacBasis: "rbac:role=FINANCE_APPROVER",
		recordErr: errors.New("db down")}, m)
	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d, want 503", w.Code)
	}
	if len(m.decisions) != 0 || m.failures != 1 {
		t.Fatalf("decisions=%d failures=%d, want 0 / 1", len(m.decisions), m.failures)
	}
}
