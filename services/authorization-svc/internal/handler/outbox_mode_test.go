package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/events"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

const outboxAuthorizeBody = `{"principal_id":"p-1","legal_entity_id":"22222222-2222-4222-8222-222222222222","action_type":"PAYMENT_APPROVE"}`

// With the outbox on, the decision's events go to the store with the decision
// (same transaction) and nothing is published directly — a Kafka error can no
// longer lose them.
func TestOutboxMode_EventsGoWithTheDecisionNotToKafka(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actions []string
	}{{"granted", []string{"PAYMENT_APPROVE"}}, {"denied", nil}} {
		s := &stubStore{rbacActions: tc.actions, rbacBasis: "rbac:role=FIN"}
		pub := &stubPublisher{}
		h := handler.New(s, pub, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop())
		h.UseOutbox(events.DecisionEvents)
		r := chi.NewRouter()
		handler.RegisterRoutes(r, h)

		req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(outboxAuthorizeBody))
		req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", tc.name, w.Code)
		}
		if s.recordedParams.Events == nil {
			t.Errorf("%s: decision recorded without its events — they are not in the outbox", tc.name)
		}
		if pub.grantedCalls+pub.deniedCalls != 0 {
			t.Errorf("%s: published directly as well (granted=%d denied=%d) — events would go out twice", tc.name, pub.grantedCalls, pub.deniedCalls)
		}
	}
}

// Without UseOutbox the direct path is unchanged.
func TestOutboxMode_OffPublishesDirectly(t *testing.T) {
	s := &stubStore{rbacActions: []string{"PAYMENT_APPROVE"}}
	pub := &stubPublisher{}
	r := newTestRouterFull(s, pub, &stubValidator{})
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(outboxAuthorizeBody))
	r.ServeHTTP(httptest.NewRecorder(), req)
	if s.recordedParams.Events != nil || pub.grantedCalls != 1 {
		t.Fatalf("direct mode: events set=%v grantedCalls=%d", s.recordedParams.Events != nil, pub.grantedCalls)
	}
}
