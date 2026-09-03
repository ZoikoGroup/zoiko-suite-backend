package deliver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/deliver"
	"zoiko.io/notification-svc/internal/domain"
)

type recordingProvider struct {
	sent    []deliver.Message
	receipt string
	err     error
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Send(_ context.Context, m deliver.Message) (string, error) {
	p.sent = append(p.sent, m)
	if p.err != nil {
		return "", p.err
	}
	return p.receipt, nil
}

func newRouter(p deliver.Provider) *deliver.Router {
	return deliver.NewRouter(p, zap.NewNop())
}

// IN_APP is delivered by the row existing. The assertion that matters is that
// no provider is consulted: routing an in-app notice through a mail server
// would make the platform's own notifications fail whenever SMTP was down.
func TestRouter_InAppTouchesNoProvider(t *testing.T) {
	p := &recordingProvider{receipt: "should not be used"}
	out := newRouter(p).Deliver(context.Background(), domain.Notification{
		Channel: domain.ChannelInApp, Subject: "Payroll finalized",
	})

	if !out.Delivered {
		t.Fatalf("an in-app notice was not delivered: %q", out.Reason)
	}
	if len(p.sent) != 0 {
		t.Fatalf("an in-app notice was handed to a delivery provider: %+v", p.sent)
	}
	if out.ProviderResponse == "" {
		t.Error("no evidence recorded for an in-app delivery")
	}
}

func TestRouter_EmailReachesTheProvider(t *testing.T) {
	p := &recordingProvider{receipt: "smtp accepted; message-id=<abc@zoiko.test>"}
	out := newRouter(p).Deliver(context.Background(), domain.Notification{
		Channel:          domain.ChannelEmail,
		RecipientAddress: "employee@example.com",
		Subject:          "Your payslip is available",
		Body:             "<p>August</p>",
		CorrelationID:    "corr-7",
	})

	if !out.Delivered {
		t.Fatalf("email was not delivered: %q", out.Reason)
	}
	if out.ProviderResponse != p.receipt {
		t.Errorf("provider evidence was not recorded: got %q want %q", out.ProviderResponse, p.receipt)
	}
	if len(p.sent) != 1 {
		t.Fatalf("expected one send, got %d", len(p.sent))
	}
	got := p.sent[0]
	if got.To != "employee@example.com" || got.Subject != "Your payslip is available" ||
		got.HTMLBody != "<p>August</p>" || got.CorrelationID != "corr-7" {
		t.Errorf("message was assembled wrong: %+v", got)
	}
}

// The state this whole change exists to remove: EMAIL with nothing behind it
// used to be reported as SENT by the stub. It must now be a visible failure
// that names what is missing.
func TestRouter_EmailWithNoProviderFailsAudibly(t *testing.T) {
	out := newRouter(nil).Deliver(context.Background(), domain.Notification{
		Channel: domain.ChannelEmail, RecipientAddress: "employee@example.com",
	})

	if out.Delivered {
		t.Fatal("EMAIL was reported delivered with no provider configured — " +
			"this is exactly the claim StubDeliverer used to make")
	}
	if !strings.Contains(out.Reason, "NOTIFICATION_EMAIL_PROVIDER") {
		t.Errorf("the failure does not name the missing setting: %q", out.Reason)
	}
}

func TestRouter_EmailWithNoAddressIsRefusedBeforeTheProvider(t *testing.T) {
	p := &recordingProvider{receipt: "unused"}
	out := newRouter(p).Deliver(context.Background(), domain.Notification{
		Channel: domain.ChannelEmail, // no RecipientAddress
	})

	if out.Delivered {
		t.Fatal("an EMAIL with no address was reported delivered")
	}
	if len(p.sent) != 0 {
		t.Error("an empty recipient was passed to the provider; the resulting SMTP error " +
			"would have been recorded as a provider failure for what is our own bug")
	}
}

func TestRouter_ProviderFailureCarriesRetryability(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{"transient", deliver.Retryable(errors.New("connection refused")), true},
		{"permanent", errors.New("550 no such user"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := newRouter(&recordingProvider{err: tc.err}).Deliver(context.Background(),
				domain.Notification{Channel: domain.ChannelEmail, RecipientAddress: "e@example.com"})

			if out.Delivered {
				t.Fatal("a provider error was reported as delivered")
			}
			if out.Retryable != tc.wantRetry {
				t.Errorf("Retryable = %v, want %v (reason %q)", out.Retryable, tc.wantRetry, out.Reason)
			}
			if !strings.Contains(out.Reason, "recording") {
				t.Errorf("the failure does not name the provider: %q", out.Reason)
			}
		})
	}
}

// SMS and WEBHOOK have no provider and are not silently reported as sent. The
// reason must say so, because a FAILED row with a vague reason is the shape
// migration 000002 had to preserve rather than delete.
func TestRouter_UnimplementedChannelsRefuseWithAReason(t *testing.T) {
	for _, channel := range []string{domain.ChannelSMS, domain.ChannelWebhook} {
		t.Run(channel, func(t *testing.T) {
			out := newRouter(&recordingProvider{}).Deliver(context.Background(),
				domain.Notification{Channel: channel})

			if out.Delivered {
				t.Fatalf("%s was reported delivered with no provider behind it", channel)
			}
			if strings.TrimSpace(out.Reason) == "" {
				t.Fatalf("%s failed with no reason recorded", channel)
			}
			if out.Retryable {
				t.Errorf("%s was marked retryable; no amount of retrying builds a provider", channel)
			}
		})
	}
}
