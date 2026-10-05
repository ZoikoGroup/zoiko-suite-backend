package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/policy"
)

type e2eSuppression struct{ suppressed bool }

func (s e2eSuppression) IsEmailSuppressed(context.Context, string, string, ledger.SenderStream, ledger.CommunicationClass) (bool, string, error) {
	return s.suppressed, "HARD_BOUNCE", nil
}

type e2eKill struct{ engaged bool }

func (k e2eKill) Check(context.Context, string, string) (bool, string) {
	return k.engaged, "operator pause"
}

func guardedRouter(t *testing.T, del *stubDeliverer, supp e2eSuppression, kill e2eKill) chi.Router {
	t.Helper()
	g, err := policy.NewDirectSendGuard(del, policy.NewPrecedenceEngine(supp, zap.NewNop()), kill, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{}, g, "tenant-abc")
}

func emailBody(corr string) map[string]any {
	b := sendBody(corr, "", "emp-1")
	b["channel"] = "EMAIL"
	return b
}

// F-03 end to end: a send to a suppressed address is recorded as a concluded
// FAILED notification and the provider is never called.
func TestSend_SuppressedRecipientIsNeverMailed(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	rr := doReq(guardedRouter(t, del, e2eSuppression{suppressed: true}, e2eKill{}), http.MethodPost, "/v1/notifications/", emailBody("corr-sup-1"), "p-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 (the refusal is recorded), got %d: %s", rr.Code, rr.Body.String())
	}
	if del.seen != nil {
		t.Fatal("the provider was called for a suppressed address")
	}
	if !strings.Contains(rr.Body.String(), "FAILED") || !strings.Contains(rr.Body.String(), "suppressed") {
		t.Fatalf("the register should show a FAILED notification that names the suppression: %s", rr.Body.String())
	}
}

func TestSend_CleanRecipientStillReachesTheProvider(t *testing.T) {
	del := &stubDeliverer{delivered: true, reason: "250"}
	rr := doReq(guardedRouter(t, del, e2eSuppression{}, e2eKill{}), http.MethodPost, "/v1/notifications/", emailBody("corr-sup-2"), "p-1")
	if rr.Code != http.StatusCreated || del.seen == nil {
		t.Fatalf("a clean recipient must be delivered: %d seen=%v %s", rr.Code, del.seen != nil, rr.Body.String())
	}
}

// A kill switch holds the notice; it is scheduled to retry, not concluded.
func TestSend_KillSwitchHoldsTheNoticeForRetry(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	rr := doReq(guardedRouter(t, del, e2eSuppression{}, e2eKill{engaged: true}), http.MethodPost, "/v1/notifications/", emailBody("corr-kill-1"), "p-1")
	if del.seen != nil {
		t.Fatal("the provider was called while the kill switch was engaged")
	}
	if strings.Contains(rr.Body.String(), `"status":"FAILED"`) {
		t.Fatalf("a kill-switch hold is retryable and must not be concluded as FAILED: %s", rr.Body.String())
	}
}

// Step 5: the sender states what kind of message this is, it is recorded on the
// notification, and a class the direct path does not offer is a 400 (INV-06, INV-07).
func TestSend_CommunicationClassIsValidatedAndRecorded(t *testing.T) {
	for _, class := range []string{"S0", "T0", "A1"} {
		del := &stubDeliverer{delivered: true}
		body := emailBody("corr-class-" + class)
		body["communication_class"] = class
		rr := doReq(guardedRouter(t, del, e2eSuppression{}, e2eKill{}), http.MethodPost, "/v1/notifications/", body, "p-1")
		if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"communication_class":"`+class+`"`) || del.seen == nil {
			t.Errorf("%s: want 201 recorded and delivered, got %d: %s", class, rr.Code, rr.Body.String())
		}
	}
	for _, class := range []string{"M1", "L1", "t0", "bogus"} {
		del := &stubDeliverer{delivered: true}
		body := emailBody("corr-bad-" + class)
		body["communication_class"] = class
		rr := doReq(guardedRouter(t, del, e2eSuppression{}, e2eKill{}), http.MethodPost, "/v1/notifications/", body, "p-1")
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_communication_class") || del.seen != nil {
			t.Errorf("%s: want 400 invalid_communication_class and no delivery, got %d seen=%v: %s", class, rr.Code, del.seen != nil, rr.Body.String())
		}
	}
}
