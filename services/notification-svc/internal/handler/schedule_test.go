package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
)

func queuedSendBody(corr string, extra map[string]any) map[string]any {
	b := map[string]any{"recipient_principal_id": "principal-2", "legal_entity_id": "le-us", "channel": "EMAIL",
		"subject": "Reminder", "body": "Your appointment is tomorrow.", "correlation_id": corr, "source_event_type": "appt.reminder", "source_reference": "appt-1"}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func rfc(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }

func TestSend_AQueuedSendIsCreatedButNotSubmitted(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	res := &stubResolver{email: "r@example.com"}
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{}, del, res, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-q1", map[string]any{"not_before": rfc(2 * time.Hour), "expires_at": rfc(48 * time.Hour)}), "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if n.Status != domain.StatusPending || n.NotBefore == nil || n.ExpiresAt == nil || n.NextAttemptAt == nil {
		t.Fatalf("a queued send should be PENDING with its timing: %+v", n)
	}
	if !n.NextAttemptAt.Equal(*n.NotBefore) {
		t.Errorf("it is due at not_before: next=%v not_before=%v", n.NextAttemptAt, n.NotBefore)
	}
	if del.seen != nil {
		t.Error("a queued send must not reach the provider now")
	}
	if res.calls != 0 {
		t.Error("a queued send resolves its recipient when it is sent, not now")
	}

	// A not_before in the past is not queued: it sends now.
	rr = doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-q2", map[string]any{"not_before": rfc(-time.Minute)}), "sender-1")
	if rr.Code != http.StatusCreated || del.seen == nil {
		t.Errorf("a past not_before sends immediately: %d seen=%v", rr.Code, del.seen != nil)
	}
}

func TestSend_InvalidTimingIsRefused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	for name, extra := range map[string]map[string]any{
		"already expired":      {"expires_at": rfc(-time.Minute)},
		"expiry before start":  {"not_before": rfc(2 * time.Hour), "expires_at": rfc(time.Hour)},
		"too far ahead":        {"not_before": rfc(100 * 24 * time.Hour)},
		"in-app with a timing": {"channel": "IN_APP", "not_before": rfc(time.Hour)},
		"not a time":           {"not_before": "tomorrow"},
	} {
		rr := doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-bad-"+name, extra), "sender-1")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestCancel_WithdrawsAQueuedSendAndNothingElse(t *testing.T) {
	authz := &stubAuthZ{}
	r := newRouterFull(newStubStore(), &stubPublisher{}, authz, &stubDeliverer{delivered: true}, &stubResolver{email: "r@example.com"}, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-c1", map[string]any{"not_before": rfc(time.Hour)}), "sender-1")
	var queued domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &queued)
	rr = doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-c2", nil), "sender-1")
	var sent domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &sent)

	// A reason is required, and the caller must be known.
	if rr := doReq(r, http.MethodPost, "/v1/notifications/"+queued.NotificationID+"/cancel", map[string]any{}, "ops-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("no reason = %d, want 400", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/v1/notifications/"+queued.NotificationID+"/cancel", map[string]any{"reason": "x"}, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("no principal = %d, want 401", rr.Code)
	}

	authz.calls = nil
	rr = doReq(r, http.MethodPost, "/v1/notifications/"+queued.NotificationID+"/cancel", map[string]any{"reason": "the appointment was moved"}, "ops-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rr.Code, rr.Body.String())
	}
	var got domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Status != domain.StatusCancelled || got.CancelledBy != "ops-1" || got.CancelReason != "the appointment was moved" {
		t.Errorf("cancellation not recorded: %+v", got)
	}
	if len(authz.calls) != 1 || authz.calls[0] != "NOTIFICATION_SEND" {
		t.Errorf("authorized as %v, want NOTIFICATION_SEND", authz.calls)
	}

	// Already withdrawn, already sent, and unknown are all refused, with the right answers.
	for name, tc := range map[string]struct {
		id   string
		want int
	}{
		"cancelled twice": {queued.NotificationID, http.StatusConflict},
		"already sent":    {sent.NotificationID, http.StatusConflict},
		"unknown":         {"does-not-exist", http.StatusNotFound},
	} {
		if rr := doReq(r, http.MethodPost, "/v1/notifications/"+tc.id+"/cancel", map[string]any{"reason": "again"}, "ops-1"); rr.Code != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", name, rr.Code, tc.want, rr.Body.String())
		}
	}
}

func TestCancel_RequiresTheRightToSendForTheEntity(t *testing.T) {
	store := newStubStore()
	ok := newRouterFull(store, &stubPublisher{}, &stubAuthZ{}, &stubDeliverer{delivered: true}, &stubResolver{email: "r@example.com"}, "tenant-abc")
	rr := doReq(ok, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-auth", map[string]any{"not_before": rfc(time.Hour)}), "sender-1")
	var queued domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &queued)

	denied := newRouterFull(store, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubDeliverer{delivered: true}, &stubResolver{email: "r@example.com"}, "tenant-abc")
	if rr := doReq(denied, http.MethodPost, "/v1/notifications/"+queued.NotificationID+"/cancel", map[string]any{"reason": "x"}, "stranger"); rr.Code != http.StatusForbidden {
		t.Errorf("a caller without the right to send = %d, want 403", rr.Code)
	}
	if store.byID[queued.NotificationID].Status != domain.StatusPending {
		t.Error("a refused cancellation must leave the communication queued")
	}
}
