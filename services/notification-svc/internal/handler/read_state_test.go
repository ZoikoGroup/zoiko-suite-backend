package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/notification-svc/internal/domain"
)

// send posts a notification and returns the created record.
func send(t *testing.T, r chi.Router, principalID string, body map[string]any) domain.Notification {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/notifications/", body, principalID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("send: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var n domain.Notification
	if err := json.Unmarshal(rr.Body.Bytes(), &n); err != nil {
		t.Fatalf("decode created notification: %v", err)
	}
	return n
}

func inAppTo(recipient, correlationID string) map[string]any {
	return map[string]any{
		"recipient_principal_id": recipient,
		"legal_entity_id":        "le-us",
		"channel":                "IN_APP",
		"subject":                "Payroll run finalized",
		"body":                   "August payroll has been finalized.",
		"correlation_id":         correlationID,
	}
}

// ── marking read ─────────────────────────────────────────────────────────────

func TestMarkRead_RecipientMarksTheirOwnNotice(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	created := send(t, r, "sender-1", inAppTo("employee-9", "corr-1"))
	if created.ReadAt != nil {
		t.Fatal("a newly created notice was already marked read")
	}

	rr := doReq(r, http.MethodPost, "/v1/notifications/"+created.NotificationID+"/read", nil, "employee-9")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.Notification
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ReadAt == nil {
		t.Fatal("read_at was not set")
	}
}

// The unread badge belongs to the recipient. An administrator holding
// NOTIFICATION_VIEW over the legal entity can read the register — that is what
// the grant is for — but reading the register is not the recipient seeing
// their notice, and it must not clear their badge.
func TestMarkRead_OnlyTheRecipientMayMark(t *testing.T) {
	store := newStubStore()
	// An authz stub that grants everything, so the refusal below cannot be
	// mistaken for a missing permission.
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	created := send(t, r, "sender-1", inAppTo("employee-9", "corr-1"))

	rr := doReq(r, http.MethodPost, "/v1/notifications/"+created.NotificationID+"/read", nil, "admin-7")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("an administrator cleared somebody else's unread notice: got %d, want 403", rr.Code)
	}

	if n, _ := store.GetNotification(t.Context(), created.NotificationID); n.ReadAt != nil {
		t.Error("read_at was set by a principal who is not the recipient")
	}
}

// Re-opening an inbox re-issues the mark. Without COALESCE in the store, each
// one would move read_at forward, and "when did they first see this" would
// decay into "when did they last look".
func TestMarkRead_KeepsTheFirstRead(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	created := send(t, r, "sender-1", inAppTo("employee-9", "corr-1"))

	first := doReq(r, http.MethodPost, "/v1/notifications/"+created.NotificationID+"/read", nil, "employee-9")
	second := doReq(r, http.MethodPost, "/v1/notifications/"+created.NotificationID+"/read", nil, "employee-9")

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("a repeated mark was not idempotent: %d then %d", first.Code, second.Code)
	}

	var a, b domain.Notification
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a.ReadAt == nil || b.ReadAt == nil || !a.ReadAt.Equal(*b.ReadAt) {
		t.Errorf("the second mark moved read_at: %v then %v", a.ReadAt, b.ReadAt)
	}
}

// This service cannot know whether an email was opened. Accepting a read mark
// on one would record an assertion it has no way to make.
func TestMarkRead_RefusedForChannelsWithNoReadState(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	created := send(t, r, "sender-1", map[string]any{
		"recipient_principal_id": "employee-9",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Your payslip is available",
		"correlation_id":         "corr-email",
	})

	rr := doReq(r, http.MethodPost, "/v1/notifications/"+created.NotificationID+"/read", nil, "employee-9")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("marking an EMAIL read returned %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

func TestMarkRead_UnknownNotification(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/notifications/00000000-0000-0000-0000-000000000000/read", nil, "employee-9")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestMarkRead_RequiresIdentity(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/notifications/some-id/read", nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// ── unread count ─────────────────────────────────────────────────────────────

func TestUnreadCount_CountsOnlyTheCallersUnreadInAppNotices(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	mine1 := send(t, r, "sender-1", inAppTo("employee-9", "corr-1"))
	send(t, r, "sender-1", inAppTo("employee-9", "corr-2"))
	send(t, r, "sender-1", inAppTo("someone-else", "corr-3"))
	// An email to the same principal: not countable, because nothing here can
	// observe whether it was opened.
	send(t, r, "sender-1", map[string]any{
		"recipient_principal_id": "employee-9",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Payslip",
		"correlation_id":         "corr-4",
	})

	if got := unreadCount(t, r, "employee-9"); got != 2 {
		t.Fatalf("unread_count = %d, want 2", got)
	}

	doReq(r, http.MethodPost, "/v1/notifications/"+mine1.NotificationID+"/read", nil, "employee-9")

	if got := unreadCount(t, r, "employee-9"); got != 1 {
		t.Fatalf("after marking one read, unread_count = %d, want 1", got)
	}
	if got := unreadCount(t, r, "someone-else"); got != 1 {
		t.Errorf("another principal's count changed: %d, want 1", got)
	}
}

func TestUnreadCount_RequiresIdentity(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/notifications/unread-count", nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// chi must not capture "unread-count" as a notification id.
func TestUnreadCount_IsNotRoutedAsANotificationID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/notifications/unread-count", nil, "employee-9")
	if rr.Code == http.StatusNotFound {
		t.Fatal("unread-count was routed to GetNotification and answered 404")
	}
}

func unreadCount(t *testing.T, r chi.Router, principalID string) int {
	t.Helper()
	rr := doReq(r, http.MethodGet, "/v1/notifications/unread-count", nil, principalID)
	if rr.Code != http.StatusOK {
		t.Fatalf("unread-count: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		UnreadCount int    `json:"unread_count"`
		Channel     string `json:"channel"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Channel != domain.ChannelInApp {
		t.Errorf("the count does not say which channel it covers: %q", body.Channel)
	}
	return body.UnreadCount
}

// ── the unread filter on the register read ───────────────────────────────────

func TestListNotifications_UnreadOnlyReachesTheStore(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	doReq(r, http.MethodGet, "/v1/notifications/?unread_only=true", nil, "employee-9")
	if !store.lastFilter.UnreadOnly {
		t.Error("unread_only=true did not reach the store filter")
	}

	doReq(r, http.MethodGet, "/v1/notifications/", nil, "employee-9")
	if store.lastFilter.UnreadOnly {
		t.Error("unread_only defaulted to true")
	}
}
