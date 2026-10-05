package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
)

type fakeInner struct{ calls int }

func (f *fakeInner) Deliver(_ context.Context, _ domain.Notification) domain.DeliveryOutcome {
	f.calls++
	return domain.DeliveryOutcome{Delivered: true, ProviderResponse: "accepted"}
}

type fakeSuppression struct {
	suppressed bool
	reason     string
	err        error
	gotStream  ledger.SenderStream
	gotClass   ledger.CommunicationClass
	gotEmail   string
	calls      int
}

func (f *fakeSuppression) IsEmailSuppressed(_ context.Context, _ string, email string, s ledger.SenderStream, c ledger.CommunicationClass) (bool, string, error) {
	f.calls++
	f.gotEmail, f.gotStream, f.gotClass = email, s, c
	return f.suppressed, f.reason, f.err
}

type fakeKill struct {
	engaged bool
	reason  string
	gotTmpl string
	gotTnnt string
	checked int
}

func (f *fakeKill) Check(_ context.Context, tenant, tmpl string) (bool, string) {
	f.checked++
	f.gotTnnt, f.gotTmpl = tenant, tmpl
	return f.engaged, f.reason
}

func email() domain.Notification {
	return domain.Notification{NotificationID: "n1", TenantID: "t1", Channel: domain.ChannelEmail, RecipientAddress: "a@example.com", TemplateID: "tpl-1"}
}

func guard(t *testing.T, in *fakeInner, s *fakeSuppression, k *fakeKill) *DirectSendGuard {
	t.Helper()
	g, err := NewDirectSendGuard(in, s, k, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDirectGuard_CleanEmailIsDelivered(t *testing.T) {
	in, s, k := &fakeInner{}, &fakeSuppression{}, &fakeKill{}
	out := guard(t, in, s, k).Deliver(context.Background(), email())
	if !out.Delivered || in.calls != 1 {
		t.Fatalf("a clean email must reach the provider exactly once: %+v calls=%d", out, in.calls)
	}
	if s.gotStream != ledger.StreamTransactional || s.gotClass != ledger.ClassT0 || s.gotEmail != "a@example.com" {
		t.Errorf("a direct send is checked as TRANSACTIONAL/T0 against its own address: %v %v %q", s.gotStream, s.gotClass, s.gotEmail)
	}
	if k.gotTnnt != "t1" || k.gotTmpl != "tpl-1" {
		t.Errorf("kill switch asked about the wrong scope: %q %q", k.gotTnnt, k.gotTmpl)
	}
}

// NP-13: a hard-bounced or complaint-suppressed address is not mailed.
func TestDirectGuard_SuppressedAddressIsTerminalAndNeverReachesTheProvider(t *testing.T) {
	in, s := &fakeInner{}, &fakeSuppression{suppressed: true, reason: "HARD_BOUNCE"}
	out := guard(t, in, s, &fakeKill{}).Deliver(context.Background(), email())
	if in.calls != 0 || out.Delivered {
		t.Fatalf("the provider must not be called for a suppressed address: %+v calls=%d", out, in.calls)
	}
	if out.Retryable || out.Unknown {
		t.Errorf("suppression is terminal; retrying cannot help: %+v", out)
	}
	if !strings.Contains(out.Reason, "HARD_BOUNCE") {
		t.Errorf("the reason should name the suppression: %q", out.Reason)
	}
}

// NP-56 / INV-30: an unreadable suppression list is not permission.
func TestDirectGuard_SuppressionLookupFailureFailsClosed(t *testing.T) {
	in := &fakeInner{}
	out := guard(t, in, &fakeSuppression{err: errors.New("db down")}, &fakeKill{}).Deliver(context.Background(), email())
	if in.calls != 0 || out.Delivered {
		t.Fatalf("a failed lookup must not fall through to the provider: %+v calls=%d", out, in.calls)
	}
	if !out.Retryable {
		t.Errorf("a lookup failure is transient and should be retried: %+v", out)
	}
}

func TestDirectGuard_KillSwitchHoldsDeliveryAndIsRetryable(t *testing.T) {
	in, s := &fakeInner{}, &fakeSuppression{}
	out := guard(t, in, s, &fakeKill{engaged: true, reason: "provider incident"}).Deliver(context.Background(), email())
	if in.calls != 0 || out.Delivered || !out.Retryable {
		t.Fatalf("an engaged kill switch pauses delivery without dropping the notice: %+v calls=%d", out, in.calls)
	}
	if !strings.Contains(out.Reason, "provider incident") {
		t.Errorf("the reason should say why: %q", out.Reason)
	}
	if s.calls != 0 {
		t.Errorf("the kill switch is checked first; no need to read the suppression list while halted")
	}
}

func TestDirectGuard_InAppAndOtherChannelsAreNotGuarded(t *testing.T) {
	for _, ch := range []string{domain.ChannelInApp, domain.ChannelWebhook, domain.ChannelSMS} {
		in, s, k := &fakeInner{}, &fakeSuppression{suppressed: true}, &fakeKill{engaged: true}
		n := email()
		n.Channel = ch
		guard(t, in, s, k).Deliver(context.Background(), n)
		if in.calls != 1 || s.calls != 0 || k.checked != 0 {
			t.Errorf("%s: only EMAIL has an address to suppress; got inner=%d suppression=%d kill=%d", ch, in.calls, s.calls, k.checked)
		}
	}
}

func TestDirectGuard_RefusesToBeBuiltWithoutItsControls(t *testing.T) {
	if _, err := NewDirectSendGuard(nil, &fakeSuppression{}, &fakeKill{}, nil); err == nil {
		t.Error("no deliverer")
	}
	if _, err := NewDirectSendGuard(&fakeInner{}, nil, &fakeKill{}, nil); err == nil {
		t.Error("no suppression checker: a guard that guards nothing must not be constructible")
	}
	if _, err := NewDirectSendGuard(&fakeInner{}, &fakeSuppression{}, nil, nil); err == nil {
		t.Error("no kill switch")
	}
}
