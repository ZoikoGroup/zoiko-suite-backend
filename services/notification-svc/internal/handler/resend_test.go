package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
)

func seededForResend(status string) *stubStore {
	s := newStubStore()
	s.byID["n-r"] = &domain.Notification{
		NotificationID: "n-r", TenantID: "tenant-abc", LegalEntityID: "le-us",
		RecipientPrincipalID: "emp-1", RecipientAddress: "emp1@example.com",
		Channel: domain.ChannelEmail, Status: status, DeliveryAttempts: 1,
		FailureReason: "550 mailbox full",
	}
	return s
}

func TestResend_RequiresAReason(t *testing.T) {
	r := newRouter(seededForResend(domain.StatusFailed), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/notifications/n-r/resend", map[string]any{}, "p-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no reason: want 400, got %d", rr.Code)
	}
}

// §6.2: UNKNOWN never authorizes a blind second send. The ambiguous attempt is
// resolved first; the resend is refused with the spec's NCD-014.
func TestResend_RefusedWhileOutcomeUnknown(t *testing.T) {
	s := seededForResend(domain.StatusPendingUnknown)
	del := &stubDeliverer{delivered: true}
	r := newRouterWith(s, &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	rr := doReq(r, http.MethodPost, "/v1/notifications/n-r/resend", map[string]any{"reason": "customer says not received"}, "p-1")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "DELIVERY_OUTCOME_UNKNOWN") {
		t.Fatalf("want 409 DELIVERY_OUTCOME_UNKNOWN, got %d: %s", rr.Code, rr.Body.String())
	}
	if del.seen != nil {
		t.Fatal("the provider was called for an unresolved ambiguous notification")
	}
}

func TestResend_RefusedWhileStillPending(t *testing.T) {
	r := newRouter(seededForResend(domain.StatusPending), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/notifications/n-r/resend", map[string]any{"reason": "x"}, "p-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("PENDING: want 409, got %d", rr.Code)
	}
}

func TestResend_AuthorizedAsSend(t *testing.T) {
	authz := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	r := newRouter(seededForResend(domain.StatusFailed), &stubPublisher{}, authz)
	rr := doReq(r, http.MethodPost, "/v1/notifications/n-r/resend", map[string]any{"reason": "x"}, "p-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied: want 403, got %d", rr.Code)
	}
	if len(authz.calls) != 1 || authz.calls[0] != "NOTIFICATION_SEND" {
		t.Fatalf("authz calls = %v, want [NOTIFICATION_SEND]", authz.calls)
	}
}

// A FAILED notification resent successfully concludes SENT on the same
// communication, having marked the submission first, and records the reason.
func TestResend_FailedThenAccepted(t *testing.T) {
	s := seededForResend(domain.StatusFailed)
	pub := &stubPublisher{}
	del := &stubDeliverer{delivered: true, reason: "250 queued as R1"}
	r := newRouterWith(s, pub, &stubAuthZ{}, del, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/n-r/resend",
		map[string]any{"reason": "mailbox cleared; customer asked again"}, "p-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.Notification
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NotificationID != "n-r" || got.Status != domain.StatusSent || got.DeliveryAttempts != 2 {
		t.Fatalf("resent notification = id %s status %s attempts %d, want n-r SENT 2", got.NotificationID, got.Status, got.DeliveryAttempts)
	}
	if got.ResendCount != 1 || got.LastResendReason == "" {
		t.Fatalf("resend summary not recorded: count=%d reason=%q", got.ResendCount, got.LastResendReason)
	}
	if len(s.submitted) != 1 || pub.sent != 1 {
		t.Fatalf("submitted=%v sent events=%d, want one of each", s.submitted, pub.sent)
	}
}

// A communication produced by the ledger pipeline is not resent through the direct
// path: the register row lacks the stream's sender identity.
func TestResend_LedgerOwnedCommunicationIsRefused(t *testing.T) {
	s := seededForResend(domain.StatusFailed)
	s.ledgerOwned = map[string]bool{"n-r": true}
	del := &stubDeliverer{delivered: true}
	r := newRouterWith(s, &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	rr := doReq(r, http.MethodPost, "/v1/notifications/n-r/resend", map[string]any{"reason": "customer asked"}, "p-1")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "ledger_communication_not_resendable") {
		t.Fatalf("want 409 ledger_communication_not_resendable, got %d: %s", rr.Code, rr.Body.String())
	}
	if del.seen != nil {
		t.Fatal("the provider was called for a ledger-owned communication")
	}
}
