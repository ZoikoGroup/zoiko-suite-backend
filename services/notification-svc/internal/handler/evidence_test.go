package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

type stubEvidence struct{ facts []domain.DeliveryEvidence }

func (s *stubEvidence) ListDeliveryEvidence(_ context.Context, id string) ([]domain.DeliveryEvidence, error) {
	out := []domain.DeliveryEvidence{}
	for _, f := range s.facts {
		if f.NotificationID == id {
			f.Limits = domain.EvidenceLimits(f.Fact)
			out = append(out, f)
		}
	}
	return out, nil
}

func evidenceRouter(t *testing.T, authz *stubAuthZ, ev handler.EvidenceStore) (chi.Router, string) {
	t.Helper()
	s := newStubStore()
	s.events = &stubPublisher{}
	n := &domain.Notification{NotificationID: "n-ev-1", TenantID: "tenant-abc", LegalEntityID: "le-us", Status: domain.StatusSent, Channel: domain.ChannelEmail}
	s.byID[n.NotificationID] = n
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(handler.Deps{Store: s, AuthZ: authz, Deliverer: &stubDeliverer{delivered: true},
		Recipient: &stubResolver{email: "r@example.com"}, Evidence: ev, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}))
	return r, n.NotificationID
}

func TestEvidenceAPI(t *testing.T) {
	ev := &stubEvidence{facts: []domain.DeliveryEvidence{{EvidenceID: "e1", NotificationID: "n-ev-1", Fact: domain.EvidenceMailboxAccepted,
		Strength: domain.StrengthMailboxLevel, OccurredAt: time.Now()}}}

	t.Run("lists facts with their limits and the notice that none proves human reading", func(t *testing.T) {
		authz := &stubAuthZ{}
		r, id := evidenceRouter(t, authz, ev)
		rr := doReq(r, http.MethodGet, "/v1/notifications/"+id+"/evidence", nil, "alice")
		if rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		for _, want := range []string{"MAILBOX_ACCEPTED", "does not prove the recipient saw it", "None of it proves a person saw"} {
			if !strings.Contains(body, want) {
				t.Errorf("response is missing %q: %s", want, body)
			}
		}
		if len(authz.calls) != 1 || authz.calls[0] != "NOTIFICATION_VIEW" {
			t.Errorf("authorized as %v, want NOTIFICATION_VIEW", authz.calls)
		}
	})
	t.Run("not configured is 503", func(t *testing.T) {
		r, id := evidenceRouter(t, &stubAuthZ{}, nil)
		if rr := doReq(r, http.MethodGet, "/v1/notifications/"+id+"/evidence", nil, "alice"); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rr.Code)
		}
	})
	t.Run("unknown notification is 404 and a caller who may not view is 403", func(t *testing.T) {
		r, _ := evidenceRouter(t, &stubAuthZ{}, ev)
		if rr := doReq(r, http.MethodGet, "/v1/notifications/nope/evidence", nil, "alice"); rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
		r, id := evidenceRouter(t, &stubAuthZ{err: domain.ErrAuthorizationDenied}, ev)
		if rr := doReq(r, http.MethodGet, "/v1/notifications/"+id+"/evidence", nil, "alice"); rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
		if rr := doReq(r, http.MethodGet, "/v1/notifications/"+id+"/evidence", nil, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("no principal: status = %d, want 401", rr.Code)
		}
	})
}
