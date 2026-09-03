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
	"zoiko.io/notification-svc/internal/retry"
)

// â”€â”€ stubs â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type stubStore struct {
	byID       map[string]*domain.Notification
	byCorr     map[string]string // correlation_id -> notification_id
	lastFilter domain.ListFilter
	scheduled  []scheduledRetry
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

func (s *stubStore) ListNotifications(_ context.Context, f domain.ListFilter) ([]domain.Notification, error) {
	s.lastFilter = f
	var out []domain.Notification
	for _, n := range s.byID {
		if f.LegalEntityID != "" && n.LegalEntityID != f.LegalEntityID {
			continue
		}
		if f.RecipientPrincipalID != "" && n.RecipientPrincipalID != f.RecipientPrincipalID {
			continue
		}
		if f.Status != "" && n.Status != f.Status {
			continue
		}
		if f.UnreadOnly && (n.ReadAt != nil || n.Channel != domain.ChannelInApp) {
			continue
		}
		out = append(out, *n)
	}
	return out, nil
}

func (s *stubStore) CompleteDelivery(_ context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time) error {
	n, ok := s.byID[id]
	if !ok {
		return domain.ErrNotificationNotFound
	}
	n.Status = newStatus
	n.FailureReason = failureReason
	n.ProviderResponse = providerResponse
	n.SentAt = sentAt
	return nil
}

// MarkRead mirrors the store's COALESCE: the first read is kept, so a repeated
// mark does not move read_at forward.
// scheduledRetry records what ScheduleRetry was asked to do, so a test can
// assert that a transient failure was rescheduled rather than concluded.
type scheduledRetry struct {
	reason        string
	nextAttemptAt time.Time
}

func (s *stubStore) ScheduleRetry(_ context.Context, id, _, failureReason string, attemptedAt, nextAttemptAt time.Time) error {
	n, ok := s.byID[id]
	if !ok {
		return domain.ErrNotificationNotFound
	}
	// Mirrors the SQL: the notification stays PENDING, the attempt count goes
	// up, and the schedule is what makes it retryable. A stub that concluded
	// the notification here would let a handler bug that marks it FAILED pass
	// unnoticed.
	n.Status = "PENDING"
	n.FailureReason = failureReason
	n.DeliveryAttempts++
	n.LastAttemptAt = &attemptedAt
	n.NextAttemptAt = &nextAttemptAt
	s.scheduled = append(s.scheduled, scheduledRetry{reason: failureReason, nextAttemptAt: nextAttemptAt})
	return nil
}

func (s *stubStore) MarkRead(_ context.Context, id, recipientPrincipalID string, readAt time.Time) error {
	n, ok := s.byID[id]
	if !ok || n.RecipientPrincipalID != recipientPrincipalID || n.Channel != domain.ChannelInApp {
		return domain.ErrNotificationNotFound
	}
	if n.ReadAt == nil {
		n.ReadAt = &readAt
	}
	return nil
}

func (s *stubStore) CountUnread(_ context.Context, recipientPrincipalID string) (int, error) {
	count := 0
	for _, n := range s.byID {
		if n.RecipientPrincipalID == recipientPrincipalID &&
			n.Channel == domain.ChannelInApp && n.ReadAt == nil {
			count++
		}
	}
	return count, nil
}

type stubPublisher struct {
	sent, failed int
}

func (p *stubPublisher) PublishSent(_ context.Context, _ string, _ domain.Notification) { p.sent++ }
func (p *stubPublisher) PublishFailed(_ context.Context, _ string, _ domain.Notification, _ string) {
	p.failed++
}

type stubAuthZ struct {
	err   error
	calls []string // actionType per call, in order
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, actionType string) error {
	a.calls = append(a.calls, actionType)
	return a.err
}

// stubDeliverer drives the delivery outcome from the test rather than from the
// channel name, so the FAILED path is exercised by a provider that genuinely
// refuses â€” which is what a real adapter failing looks like.
type stubDeliverer struct {
	delivered bool
	reason    string

	// retryable makes the refusal a transient one, which the handler must
	// schedule rather than conclude.
	retryable bool

	// seen records the notification handed over, so a test can assert what the
	// transport was actually given â€” the resolved address in particular, which
	// is the difference between a message addressed to somebody and one the
	// service merely recorded.
	seen *domain.Notification
}

func (d *stubDeliverer) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.seen = &n
	if !d.delivered {
		return domain.DeliveryOutcome{Reason: d.reason, Retryable: d.retryable}
	}
	return domain.DeliveryOutcome{Delivered: true, ProviderResponse: d.reason}
}

// stubResolver stands in for identity-context-svc.
type stubResolver struct {
	email string
	err   error
	calls int
}

func (s *stubResolver) ResolveEmail(_ context.Context, _, _, _ string) (string, error) {
	s.calls++
	return s.email, s.err
}

// â”€â”€ router factory â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	return newRouterWith(s, pub, authz, &stubDeliverer{delivered: true, reason: "delivered via stub"}, "tenant-abc")
}

// newRouterWith supplies a resolver that always succeeds, so the tests that
// predate recipient resolution keep testing what they were written to test.
// The resolution paths have their own tests below, which pass an explicit one.
func newRouterWith(s *stubStore, pub *stubPublisher, authz *stubAuthZ, del handler.Deliverer, tenantID string) chi.Router {
	return newRouterFull(s, pub, authz, del, &stubResolver{email: "recipient@example.com"}, tenantID)
}

func newRouterFull(s *stubStore, pub *stubPublisher, authz *stubAuthZ, del handler.Deliverer,
	res handler.RecipientResolver, tenantID string) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenantID != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenantID))
			}
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(handler.Deps{
		Store:     s,
		Publisher: pub,
		AuthZ:     authz,
		Deliverer: del,
		Recipient: res,
		// The default policy for these tests. Retryable failures therefore
		// schedule rather than conclude, which is what the service does — the
		// retry-specific cases below pin the policy explicitly.
		RetryPolicy: retry.DefaultPolicy,
		Log:         zap.NewNop(),
	})
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

// â”€â”€ SendNotification tests â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

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

// An unrecognised channel is a caller mistake caught at the boundary. It used
// to reach the delivery adapter, which reported it as a delivery failure â€” so
// a typo produced a stored FAILED record and a notification.failed event,
// evidence of an attempt no provider ever saw.
func TestSendNotification_UnsupportedChannel_IsRejectedNotRecordedAsFailedDelivery(t *testing.T) {
	pub := &stubPublisher{}
	store := newStubStore()
	r := newRouter(store, pub, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "CARRIER_PIGEON",
		"subject":                "Test",
		"correlation_id":         "corr-bad-channel",
	}, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(store.byID) != 0 {
		t.Errorf("a rejected channel must not leave a notification record, found %d", len(store.byID))
	}
	if pub.failed != 0 {
		t.Errorf("a rejected channel must not publish notification.failed, got %d", pub.failed)
	}
}

// A genuine delivery failure still answers 201: the service's own critical
// constraint (03-microservices.md Â§9.7) is that a notification failure must
// not collapse the workflow that raised it.
func TestSendNotification_DeliveryRefused_RecordsFailedButStill201(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouterWith(newStubStore(), pub, &stubAuthZ{},
		&stubDeliverer{delivered: false, reason: "provider rejected the recipient address"}, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Test",
		"correlation_id":         "corr-failed",
	}, "principal-1")

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

// A transient refusal must NOT conclude the notification. It stays PENDING
// with a schedule on it for internal/retry's worker to pick up, and publishes
// nothing — a notification.failed here followed by a notification.sent two
// minutes later would have consumers react to an outcome that never happened.
//
// This is the behaviour the Retryable flag existed for and did not have: before
// the worker landed, deliver classified transient failures faithfully and the
// handler concluded every one of them FAILED regardless.
func TestSendNotification_TransientFailure_IsScheduledNotConcluded(t *testing.T) {
	store := newStubStore()
	pub := &stubPublisher{}
	r := newRouterWith(store, pub, &stubAuthZ{},
		&stubDeliverer{delivered: false, retryable: true, reason: "dial tcp: connection refused"},
		"tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Payslip available",
		"correlation_id":         "corr-transient",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var n domain.Notification
	_ = json.NewDecoder(rr.Body).Decode(&n)

	if n.Status != "PENDING" {
		t.Errorf("status = %q, want PENDING — a scheduled retry has not concluded", n.Status)
	}
	if n.NextAttemptAt == nil {
		t.Error("next_attempt_at is nil; nothing would ever re-attempt this notification")
	}
	if !n.Retrying() {
		t.Error("Retrying() is false for a notification that is PENDING with a schedule")
	}
	if n.FailureReason == "" {
		t.Error("the reason for the failed attempt should still be on the record")
	}
	if n.DeliveryAttempts != 1 {
		t.Errorf("delivery_attempts = %d, want 1", n.DeliveryAttempts)
	}
	if len(store.scheduled) != 1 {
		t.Fatalf("store.scheduled = %v, want exactly one scheduled retry", store.scheduled)
	}
	if pub.sent != 0 || pub.failed != 0 {
		t.Errorf("published sent=%d failed=%d, want nothing published while a retry is pending",
			pub.sent, pub.failed)
	}
}

// A settled refusal — a mailbox that does not exist — must conclude at once
// rather than consume the retry budget waiting for an answer that will not
// change.
func TestSendNotification_SettledFailure_ConcludesImmediately(t *testing.T) {
	store := newStubStore()
	pub := &stubPublisher{}
	r := newRouterWith(store, pub, &stubAuthZ{},
		&stubDeliverer{delivered: false, retryable: false, reason: "550 no such mailbox"},
		"tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Payslip available",
		"correlation_id":         "corr-settled",
	}, "principal-1")

	var n domain.Notification
	_ = json.NewDecoder(rr.Body).Decode(&n)

	if n.Status != "FAILED" {
		t.Errorf("status = %q, want FAILED", n.Status)
	}
	if n.NextAttemptAt != nil {
		t.Error("a permanent refusal must not be scheduled for another attempt")
	}
	if len(store.scheduled) != 0 {
		t.Errorf("store.scheduled = %v, want nothing scheduled", store.scheduled)
	}
	if pub.failed != 1 {
		t.Errorf("published failed=%d, want 1 for a concluded failure", pub.failed)
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
	// A retry must not re-send â€” exactly one sent event across both requests.
	if pub.sent != 1 {
		t.Errorf("expected exactly 1 sent event across both requests, got %d", pub.sent)
	}
}

// â”€â”€ GetNotification / ListNotifications tests â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

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

// â”€â”€ the gaps closed in this pass â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// The authorization used to be conditional on the filter, so omitting
// legal_entity_id â€” the easier request â€” returned the whole tenant's
// notifications to a principal holding no grant at all.
func TestListNotifications_WithoutLegalEntity_IsScopedToCallersOwnInbox(t *testing.T) {
	store := newStubStore()
	authz := &stubAuthZ{}
	r := newRouter(store, &stubPublisher{}, authz)

	rr := doReq(r, http.MethodGet, "/v1/notifications/", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if store.lastFilter.RecipientPrincipalID != "principal-1" {
		t.Errorf("an unscoped list must be forced to the caller's own inbox, got recipient filter %q",
			store.lastFilter.RecipientPrincipalID)
	}
}

func TestListNotifications_OtherRecipientWithoutLegalEntity_IsRefused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/notifications/?recipient_principal_id=someone-else", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 reading another principal's inbox unscoped, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListNotifications_WithLegalEntity_IsAuthorized(t *testing.T) {
	authz := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	r := newRouter(newStubStore(), &stubPublisher{}, authz)
	rr := doReq(r, http.MethodGet, "/v1/notifications/?legal_entity_id=le-us", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
	if len(authz.calls) != 1 || authz.calls[0] != "NOTIFICATION_VIEW" {
		t.Errorf("expected one NOTIFICATION_VIEW check, got %v", authz.calls)
	}
}

// A missing tenant scope used to be noticed first by the store, which reported
// it as 503 store_unavailable â€” an outage status for a forgotten header.
func TestRequests_WithoutTenantScope_Are401NotServiceUnavailable(t *testing.T) {
	r := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		&stubDeliverer{delivered: true}, "")

	for _, tc := range []struct{ name, method, path string }{
		{"list", http.MethodGet, "/v1/notifications/"},
		{"get", http.MethodGet, "/v1/notifications/some-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doReq(r, tc.method, tc.path, nil, "principal-1")
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Test",
		"correlation_id":         "corr-no-tenant",
	}, "principal-1")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("send: expected 401 got %d: %s", rr.Code, rr.Body.String())
	}
}

// A misspelled field used to be discarded silently, so the caller got a 201
// for a notification that did not say what they wrote.
func TestSendNotification_UnknownField_IsRejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subjekt":                "typo",
		"subject":                "Real subject",
		"correlation_id":         "corr-unknown-field",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListNotifications_PagingIsValidated(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=100000", "?offset=-1"} {
		rr := doReq(r, http.MethodGet, "/v1/notifications/"+q, nil, "principal-1")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400 got %d", q, rr.Code)
		}
	}

	rr := doReq(r, http.MethodGet, "/v1/notifications/", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if store.lastFilter.Limit != 100 {
		t.Errorf("expected a bounded default limit of 100, got %d", store.lastFilter.Limit)
	}
}
