package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	byID   map[string]*domain.Notification
	byCorr map[string]string // correlation_id -> notification_id
}

func newStubStore() *stubStore {
	return &stubStore{
		byID:   make(map[string]*domain.Notification),
		byCorr: make(map[string]string),
	}
}

func (s *stubStore) CreateNotification(_ context.Context, n *domain.Notification) (bool, error) {
	if id, ok := s.byCorr[n.CorrelationID]; ok {
		*n = *s.byID[id]
		return false, nil
	}
	s.byID[n.NotificationID] = n
	s.byCorr[n.CorrelationID] = n.NotificationID
	return true, nil
}

func (s *stubStore) GetNotification(_ context.Context, id string) (*domain.Notification, error) {
	n, ok := s.byID[id]
	if !ok {
		return nil, domain.ErrNotificationNotFound
	}
	return n, nil
}

func (s *stubStore) ListNotifications(_ context.Context, legalEntityID, recipientPrincipalID, status string) ([]domain.Notification, error) {
	var out []domain.Notification
	for _, n := range s.byID {
		if legalEntityID != "" && n.LegalEntityID != legalEntityID {
			continue
		}
		if recipientPrincipalID != "" && n.RecipientPrincipalID != recipientPrincipalID {
			continue
		}
		if status != "" && n.Status != status {
			continue
		}
		out = append(out, *n)
	}
	return out, nil
}

func (s *stubStore) CompleteDelivery(_ context.Context, id, newStatus, failureReason string, sentAt *time.Time) error {
	n, ok := s.byID[id]
	if !ok {
		return domain.ErrNotificationNotFound
	}
	n.Status = newStatus
	n.FailureReason = failureReason
	n.SentAt = sentAt
	return nil
}

type stubPublisher struct {
	sent, failed int
}

func (p *stubPublisher) PublishSent(_ context.Context, _ string, _ domain.Notification) { p.sent++ }
func (p *stubPublisher) PublishFailed(_ context.Context, _ string, _ domain.Notification, _ string) {
	p.failed++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── SendNotification tests ─────────────────────────────────────────────────────

func TestSendNotification_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Approval needed",
		"correlation_id":         "corr-1",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestSendNotification_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Approval needed",
		"correlation_id":         "corr-1",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestSendNotification_SupportedChannel_Sent(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Invoice approval required",
		"body":                   "Invoice INV-100 needs your approval.",
		"correlation_id":         "corr-sent",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var n domain.Notification
	if err := json.NewDecoder(rr.Body).Decode(&n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n.Status != "SENT" {
		t.Errorf("expected SENT got %q", n.Status)
	}
	if n.SentAt == nil {
		t.Error("expected sent_at to be set")
	}
	if pub.sent != 1 || pub.failed != 0 {
		t.Errorf("expected 1 sent event, 0 failed, got sent=%d failed=%d", pub.sent, pub.failed)
	}
}

func TestSendNotification_UnsupportedChannel_Failed(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "CARRIER_PIGEON",
		"subject":                "Test",
		"correlation_id":         "corr-failed",
	}, "principal-1")

	// Delivery failure must not surface as an error status — the request
	// itself succeeded (a notification record was created and processed),
	// only the delivery outcome was FAILED. This is the service's own
	// critical constraint: notification failure must not collapse the
	// caller's own operation.
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 (delivery failure is not a request failure) got %d: %s", rr.Code, rr.Body.String())
	}
	var n domain.Notification
	_ = json.NewDecoder(rr.Body).Decode(&n)
	if n.Status != "FAILED" {
		t.Errorf("expected FAILED got %q", n.Status)
	}
	if n.FailureReason == "" {
		t.Error("expected a failure_reason to be recorded")
	}
	if pub.failed != 1 || pub.sent != 0 {
		t.Errorf("expected 1 failed event, 0 sent, got sent=%d failed=%d", pub.sent, pub.failed)
	}
}

func TestSendNotification_IdempotentReplay(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})

	body := map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Reminder",
		"correlation_id":         "corr-retry",
	}

	rr1 := doReq(r, http.MethodPost, "/v1/notifications/", body, "principal-1")
	var n1 domain.Notification
	_ = json.NewDecoder(rr1.Body).Decode(&n1)

	rr2 := doReq(r, http.MethodPost, "/v1/notifications/", body, "principal-1")
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200 (replay) got %d: %s", rr2.Code, rr2.Body.String())
	}
	var n2 domain.Notification
	_ = json.NewDecoder(rr2.Body).Decode(&n2)

	if n2.NotificationID != n1.NotificationID {
		t.Fatalf("retried send resolved to a different notification_id (%s) than the original (%s)", n2.NotificationID, n1.NotificationID)
	}
	// A retry must not re-send — exactly one sent event across both requests.
	if pub.sent != 1 {
		t.Errorf("expected exactly 1 sent event across both requests, got %d", pub.sent)
	}
}

// ── GetNotification / ListNotifications tests ─────────────────────────────────

func TestGetNotification_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/notifications/does-not-exist", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}

func TestListNotifications_EmptyIsEmptyArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/notifications/", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if rr.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", rr.Body.String())
	}
}
