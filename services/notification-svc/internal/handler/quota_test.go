package handler_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/quota"
)

// ZS-SVC-Y-001 6.5: a send over a budget is refused loudly, with when to retry.
func TestSend_OverAQuotaIsRefusedWith429AndRetryAfter(t *testing.T) {
	store := newStubStore()
	store.createErr = fmt.Errorf("create: %w", &quota.ExceededError{Dimension: quota.DimRecipient, Limit: 20, Window: time.Hour, RetryAfter: 754 * time.Second})
	del := &stubDeliverer{delivered: true}
	r := newRouterFull(store, &stubPublisher{}, &stubAuthZ{}, del, &stubResolver{email: "r@example.com"}, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-quota-1", nil), "sender-1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "754" {
		t.Errorf("Retry-After = %q, want 754", got)
	}
	e := decodeAPIError(t, rr.Body.Bytes())
	if e.ErrorCode != "quota_exceeded" || e.Message == "" {
		t.Errorf("unexpected body: %+v", e)
	}
	if want := "recipient"; !contains(e.Message, want) {
		t.Errorf("the refusal should name the budget: %q", e.Message)
	}
	if del.seen != nil {
		t.Error("a refused send must never reach the provider")
	}
}

// The direct send API is the one caller that asks to be counted.
func TestSend_AsksToBeCountedAgainstTheQuotas(t *testing.T) {
	store := newStubStore()
	r := newRouterFull(store, &stubPublisher{}, &stubAuthZ{}, &stubDeliverer{delivered: true}, &stubResolver{email: "r@example.com"}, "tenant-abc")
	if rr := doReq(r, http.MethodPost, "/v1/notifications/", queuedSendBody("corr-quota-2", nil), "sender-1"); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}
	if !store.countedCtx {
		t.Error("the send API must create its communication under a counting context")
	}
}

// Notice dispatch is a regulated send: it is not refused for volume it did not cause.
func TestNoticeDispatch_IsNotCountedAgainstTheQuotas(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, n := rig.create(t, nil)
	if rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+n.NoticeID+"/dispatch", nil, "sender-1"); rr.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d %s", rr.Code, rr.Body.String())
	}
	if rig.store.countedCtx {
		t.Error("a regulated notice's delivery must not be counted against the send quotas")
	}
}
