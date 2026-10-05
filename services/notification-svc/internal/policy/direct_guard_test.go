package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/privacy"
)

type fakeInner struct{ calls int }

func (f *fakeInner) Deliver(_ context.Context, _ domain.Notification) domain.DeliveryOutcome {
	f.calls++
	return domain.DeliveryOutcome{Delivered: true, ProviderResponse: "accepted"}
}

// fakePolicy stands in for the precedence engine and records what it was asked.
type fakePolicy struct {
	decision  ledger.PolicyDecision
	err       error
	calls     int
	gotIntent ledger.MessageIntent
	gotStream ledger.SenderStream
}

func (f *fakePolicy) Evaluate(_ context.Context, in *ledger.MessageIntent, s ledger.SenderStream) (ledger.PolicyDecision, error) {
	f.calls++
	f.gotIntent, f.gotStream = *in, s
	return f.decision, f.err
}

func allowed() *fakePolicy { return &fakePolicy{decision: ledger.PolicyDecision{Allowed: true}} }

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
	return domain.Notification{NotificationID: "n1", TenantID: "t1", LegalEntityID: "le1", RecipientPrincipalID: "p1",
		Channel: domain.ChannelEmail, RecipientAddress: "a@example.com", TemplateID: "tpl-1"}
}

func guard(t *testing.T, in *fakeInner, p *fakePolicy, k *fakeKill) *DirectSendGuard {
	t.Helper()
	g, err := NewDirectSendGuard(in, p, k, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDirectGuard_CleanEmailIsDeliveredAndJudgedAsT0ByDefault(t *testing.T) {
	in, p, k := &fakeInner{}, allowed(), &fakeKill{}
	out := guard(t, in, p, k).Deliver(context.Background(), email())
	if !out.Delivered || in.calls != 1 {
		t.Fatalf("a clean email must reach the provider exactly once: %+v calls=%d", out, in.calls)
	}
	if p.gotStream != ledger.StreamTransactional || p.gotIntent.CommunicationClass != ledger.ClassT0 || p.gotIntent.RecipientEmail != "a@example.com" {
		t.Errorf("a direct send that states no class is judged as TRANSACTIONAL/T0 against its own address: %v %v %q",
			p.gotStream, p.gotIntent.CommunicationClass, p.gotIntent.RecipientEmail)
	}
	if p.gotIntent.TenantID != "t1" {
		t.Errorf("the engine must be asked about the right tenant: %q", p.gotIntent.TenantID)
	}
	if k.gotTnnt != "t1" || k.gotTmpl != "tpl-1" {
		t.Errorf("kill switch asked about the wrong scope: %q %q", k.gotTnnt, k.gotTmpl)
	}
}

// Step 5: the message's own class selects the stream and reaches the shared engine.
func TestDirectGuard_StatedClassSelectsTheStream(t *testing.T) {
	for class, stream := range map[string]ledger.SenderStream{
		"S0": ledger.StreamCritical, "T0": ledger.StreamTransactional, "A1": ledger.StreamOperational,
	} {
		in, p := &fakeInner{}, allowed()
		n := email()
		n.CommunicationClass = class
		guard(t, in, p, &fakeKill{}).Deliver(context.Background(), n)
		if p.gotStream != stream || string(p.gotIntent.CommunicationClass) != class || in.calls != 1 {
			t.Errorf("%s: stream=%v class=%v delivered=%d", class, p.gotStream, p.gotIntent.CommunicationClass, in.calls)
		}
	}
}

// INV-07: marketing and lifecycle are not a direct send, even if a row carries the class.
func TestDirectGuard_MarketingAndLifecycleAreRefusedWithoutAskingThePolicy(t *testing.T) {
	for _, class := range []string{"M1", "L1", "X9"} {
		in, p := &fakeInner{}, allowed()
		n := email()
		n.CommunicationClass = class
		out := guard(t, in, p, &fakeKill{}).Deliver(context.Background(), n)
		if in.calls != 0 || out.Delivered || out.Retryable {
			t.Errorf("%s: the provider must not be called and the refusal is terminal: %+v calls=%d", class, out, in.calls)
		}
		if !strings.Contains(out.Reason, "ledger pipeline") {
			t.Errorf("%s: the reason should point to the ledger pipeline: %q", class, out.Reason)
		}
	}
}

// NP-13: a policy refusal is terminal and the provider is not called.
func TestDirectGuard_PolicyRefusalIsTerminalAndNeverReachesTheProvider(t *testing.T) {
	in := &fakeInner{}
	p := &fakePolicy{decision: ledger.PolicyDecision{Allowed: false, RuleName: "SUPPRESSION_ENFORCED", Reason: "address suppressed due to HARD_BOUNCE"}}
	out := guard(t, in, p, &fakeKill{}).Deliver(context.Background(), email())
	if in.calls != 0 || out.Delivered {
		t.Fatalf("the provider must not be called for a refused message: %+v calls=%d", out, in.calls)
	}
	if out.Retryable || out.Unknown {
		t.Errorf("a policy refusal is terminal; retrying cannot help: %+v", out)
	}
	if !strings.Contains(out.Reason, "HARD_BOUNCE") || !strings.Contains(out.Reason, "SUPPRESSION_ENFORCED") {
		t.Errorf("the reason should name the rule and the cause: %q", out.Reason)
	}
}

// NP-56 / INV-30: a policy that cannot be evaluated is not permission.
func TestDirectGuard_PolicyEvaluationFailureFailsClosed(t *testing.T) {
	in := &fakeInner{}
	out := guard(t, in, &fakePolicy{err: errors.New("db down")}, &fakeKill{}).Deliver(context.Background(), email())
	if in.calls != 0 || out.Delivered {
		t.Fatalf("a failed evaluation must not fall through to the provider: %+v calls=%d", out, in.calls)
	}
	if !out.Retryable {
		t.Errorf("a failure to evaluate is transient and should be retried: %+v", out)
	}
}

func TestDirectGuard_KillSwitchHoldsDeliveryAndIsRetryable(t *testing.T) {
	in, p := &fakeInner{}, allowed()
	out := guard(t, in, p, &fakeKill{engaged: true, reason: "provider incident"}).Deliver(context.Background(), email())
	if in.calls != 0 || out.Delivered || !out.Retryable {
		t.Fatalf("an engaged kill switch pauses delivery without dropping the notice: %+v calls=%d", out, in.calls)
	}
	if !strings.Contains(out.Reason, "provider incident") {
		t.Errorf("the reason should say why: %q", out.Reason)
	}
	if p.calls != 0 {
		t.Errorf("the kill switch is checked first; no need to evaluate policy while halted")
	}
}

func TestDirectGuard_InAppAndOtherChannelsAreNotGuarded(t *testing.T) {
	for _, ch := range []string{domain.ChannelInApp, domain.ChannelWebhook, domain.ChannelSMS} {
		in, p, k := &fakeInner{}, &fakePolicy{decision: ledger.PolicyDecision{Allowed: false}}, &fakeKill{engaged: true}
		n := email()
		n.Channel = ch
		guard(t, in, p, k).Deliver(context.Background(), n)
		if in.calls != 1 || p.calls != 0 || k.checked != 0 {
			t.Errorf("%s: only EMAIL has an address to judge; got inner=%d policy=%d kill=%d", ch, in.calls, p.calls, k.checked)
		}
	}
}

func TestDirectGuard_RefusesToBeBuiltWithoutItsControls(t *testing.T) {
	if _, err := NewDirectSendGuard(nil, allowed(), &fakeKill{}, nil); err == nil {
		t.Error("no deliverer")
	}
	if _, err := NewDirectSendGuard(&fakeInner{}, nil, &fakeKill{}, nil); err == nil {
		t.Error("no policy resolver: a guard that guards nothing must not be constructible")
	}
	if _, err := NewDirectSendGuard(&fakeInner{}, allowed(), nil, nil); err == nil {
		t.Error("no kill switch")
	}
}

// The mapping the guard relies on, and the classes the direct path accepts, agree.
func TestDirectPathClassesAndStreamsAgree(t *testing.T) {
	for _, c := range domain.DirectPathClasses {
		if _, ok := ledger.StreamForDirectClass(ledger.CommunicationClass(c)); !ok {
			t.Errorf("class %s is accepted by the API but has no stream", c)
		}
	}
	for _, c := range []ledger.CommunicationClass{ledger.ClassL1, ledger.ClassM1} {
		if _, ok := ledger.StreamForDirectClass(c); ok || domain.ValidDirectPathClass(string(c)) {
			t.Errorf("class %s must not be available on the direct path", c)
		}
	}
	// Every seed template's class/stream pair is consistent with the mapping where one exists.
	for _, d := range ledger.DefaultSeedDefinitions() {
		if s, ok := ledger.StreamForDirectClass(d.CommunicationClass); ok && s != d.SenderStream {
			t.Errorf("template %s: class %s is on stream %s but the direct path would use %s", d.TemplateKey, d.CommunicationClass, d.SenderStream, s)
		}
	}
}

type fakePrivacy struct {
	out   privacy.Outcome
	calls int
}

func (f *fakePrivacy) Check(_ context.Context, _ domain.Notification) privacy.Outcome {
	f.calls++
	return f.out
}

func TestDirectGuard_PrivacyGate(t *testing.T) {
	permit := privacy.Outcome{Applies: true, Verdict: privacy.Verdict{Allow: true, Result: "PERMIT", DecisionID: "d-1"}}
	deny := privacy.Outcome{Applies: true, Verdict: privacy.Verdict{Result: "BLOCK", DecisionID: "d-2", Reason: "NCD-008 blocked"}}
	retry := privacy.Outcome{Applies: true, Verdict: privacy.Verdict{Result: "UNAVAILABLE", Retryable: true, Reason: "NCD-008 down"}}

	t.Run("permit delivers and carries the decision as evidence", func(t *testing.T) {
		in, pg := &fakeInner{}, &fakePrivacy{out: permit}
		out := guard(t, in, allowed(), &fakeKill{}).WithPrivacyGate(pg).Deliver(context.Background(), email())
		assert.True(t, out.Delivered)
		assert.Equal(t, "d-1", out.PrivacyDecisionID)
		assert.Equal(t, "PERMIT", out.PrivacyResult)
		assert.Equal(t, 1, in.calls)
	})
	t.Run("refusal never reaches the provider and keeps the decision", func(t *testing.T) {
		in := &fakeInner{}
		out := guard(t, in, allowed(), &fakeKill{}).WithPrivacyGate(&fakePrivacy{out: deny}).Deliver(context.Background(), email())
		assert.False(t, out.Delivered)
		assert.False(t, out.Retryable)
		assert.Zero(t, in.calls)
		assert.Equal(t, "d-2", out.PrivacyDecisionID)
		assert.Equal(t, "BLOCK", out.PrivacyResult)
		assert.Contains(t, out.Reason, "NCD-008")
	})
	t.Run("an outage is retryable and recorded as UNAVAILABLE", func(t *testing.T) {
		in := &fakeInner{}
		out := guard(t, in, allowed(), &fakeKill{}).WithPrivacyGate(&fakePrivacy{out: retry}).Deliver(context.Background(), email())
		assert.True(t, out.Retryable)
		assert.Zero(t, in.calls)
		assert.Equal(t, "UNAVAILABLE", out.PrivacyResult)
	})
	t.Run("no intent: legacy send passes untouched", func(t *testing.T) {
		in := &fakeInner{}
		out := guard(t, in, allowed(), &fakeKill{}).WithPrivacyGate(&fakePrivacy{}).Deliver(context.Background(), email())
		assert.True(t, out.Delivered)
		assert.Empty(t, out.PrivacyResult)
	})
	t.Run("local refusals come first, so the remote question is not asked", func(t *testing.T) {
		pg := &fakePrivacy{out: permit}
		g := guard(t, &fakeInner{}, &fakePolicy{decision: ledger.PolicyDecision{Allowed: false, RuleName: "r"}}, &fakeKill{}).WithPrivacyGate(pg)
		out := g.Deliver(context.Background(), email())
		assert.False(t, out.Delivered)
		assert.Zero(t, pg.calls)
	})
	t.Run("in-app is not gated", func(t *testing.T) {
		pg := &fakePrivacy{out: deny}
		n := email()
		n.Channel = domain.ChannelInApp
		out := guard(t, &fakeInner{}, allowed(), &fakeKill{}).WithPrivacyGate(pg).Deliver(context.Background(), n)
		assert.True(t, out.Delivered)
		assert.Zero(t, pg.calls)
	})
}
