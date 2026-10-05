package ncd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
)

type recordingEmail struct {
	calls int
	last  domain.Notification
}

func (r *recordingEmail) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	r.calls++
	r.last = n
	return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp", ProviderResponse: "250 ok message-id=<m1>"}
}

type fakeLinker struct{ err error }

func (f fakeLinker) Headers(tenantID, email string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]string{
		"List-Unsubscribe":      "<https://notify.example.test/v1/notifications/unsubscribe?token=sealed-" + tenantID + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}, nil
}

var smtpBinding = Binding{BindingID: "smtp-primary", Channel: ChannelEmail, ProviderName: "smtp"}

func msg(purpose PurposeClass) Message {
	return Message{TenantID: "t1", CommunicationID: "c1", AttemptID: "a1", IdempotencyToken: "tok",
		Channel: ChannelEmail, To: "pat@example.test", Subject: "s", Body: "b", PurposeClass: purpose}
}

// §11.1 / INV-25: marketing email sent through the control plane carries the
// one-click unsubscribe link. It used to carry none.
func TestRouterTransport_MarketingCarriesOneClickUnsubscribe(t *testing.T) {
	em := &recordingEmail{}
	out := RouterTransport{Email: em, Unsubscribe: fakeLinker{}}.Submit(context.Background(), smtpBinding, msg(PurposeMarketing))
	if !out.Accepted || em.calls != 1 {
		t.Fatalf("marketing with a linker must be submitted: %+v calls=%d", out, em.calls)
	}
	if !strings.Contains(em.last.Headers["List-Unsubscribe"], "token=sealed-t1") ||
		em.last.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("missing RFC 8058 headers: %v", em.last.Headers)
	}
	if em.last.Headers["X-Zoiko-Idempotency-Token"] != "tok" {
		t.Fatalf("the provider idempotency token must still be carried: %v", em.last.Headers)
	}
}

// Without a way to issue the link, marketing email is refused before any
// provider is called — a known, non-retryable failure, never UNKNOWN.
func TestRouterTransport_MarketingWithoutUnsubscribeIsRefused(t *testing.T) {
	for name, tr := range map[string]RouterTransport{
		"not configured": {Unsubscribe: nil},
		"issuer fails":   {Unsubscribe: fakeLinker{err: errors.New("boom")}},
	} {
		em := &recordingEmail{}
		tr.Email = em
		out := tr.Submit(context.Background(), smtpBinding, msg(PurposeMarketing))
		if out.Accepted || out.Unknown || out.Retryable || em.calls != 0 {
			t.Errorf("%s: want refused before the provider, got %+v calls=%d", name, out, em.calls)
		}
		if !strings.HasPrefix(out.Reason, string(NCD011NoCompliantChannel)) {
			t.Errorf("%s: reason should carry NCD-011, got %q", name, out.Reason)
		}
	}
}

// Non-marketing purposes are not list mail: no unsubscribe link, and they are
// not held hostage to unsubscribe configuration.
func TestRouterTransport_NonMarketingHasNoUnsubscribeHeader(t *testing.T) {
	em := &recordingEmail{}
	out := RouterTransport{Email: em}.Submit(context.Background(), smtpBinding, msg(PurposeTransactional))
	if !out.Accepted || em.calls != 1 {
		t.Fatalf("transactional mail must not need unsubscribe configuration: %+v", out)
	}
	if _, ok := em.last.Headers["List-Unsubscribe"]; ok {
		t.Fatalf("transactional mail must not carry List-Unsubscribe: %v", em.last.Headers)
	}
}
