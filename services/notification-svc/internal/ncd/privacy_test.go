package ncd

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/notification-svc/internal/privacy"
)

type fakeDecider struct {
	d   *privacy.Decision
	err error
	got privacy.Request
	n   int
}

func (f *fakeDecider) Decide(_ context.Context, r privacy.Request) (*privacy.Decision, error) {
	f.got = r
	f.n++
	return f.d, f.err
}

func boundIntent() Intent {
	return Intent{PrivacyActivityID: "act-1", PrivacyPurposeID: "purp-1"}
}

func resolve(t *testing.T, f *fakeDecider, in Intent, supplied PermissionDecision) privacyResolution {
	t.Helper()
	s := NewService(nil, nil, nil, DefaultLimits(), nil)
	if f != nil {
		s.SetPrivacyDecider(f)
	}
	return s.resolvePrivacy(context.Background(), Actor{TenantID: "t1", PrincipalID: "alice", CorrelationID: "c1"}, in,
		&RecipientPlan{RecipientPrincipalID: "rcpt-1"}, supplied)
}

// S7-B1: with a decider wired, the permission is asked of PRV for the recipient and the intent's binding, and a
// caller-supplied value is ignored.
func TestResolvePrivacy_AsksPRVAndIgnoresTheCaller(t *testing.T) {
	f := &fakeDecider{d: &privacy.Decision{DecisionID: "prv-9", Result: privacy.ResultBlock}}
	r := resolve(t, f, boundIntent(), PermissionDecision{Decision: "PERMIT", DecisionID: "forged"})
	if r.Permission.Decision != "DENY" || r.Permission.DecisionID != "prv-9" || r.Source != "PRV" {
		t.Fatalf("a PRV BLOCK must win over a caller PERMIT: %+v", r)
	}
	if f.got.SubjectRef != "rcpt-1" || f.got.ActivityID != "act-1" || f.got.PurposeID != "purp-1" || f.got.TenantID != "t1" || f.got.PrincipalID != "alice" {
		t.Fatalf("the question must name the recipient, the intent's binding and the tenant: %+v", f.got)
	}
	if r.Note == "" {
		t.Fatal("ignoring a caller-supplied permission must be recorded")
	}
}

func TestResolvePrivacy_MapsEveryResultAndFailsClosed(t *testing.T) {
	cases := map[string]struct {
		d    *privacy.Decision
		err  error
		want string
	}{
		"permit":        {&privacy.Decision{DecisionID: "d", Result: privacy.ResultPermit}, nil, "PERMIT"},
		"block":         {&privacy.Decision{DecisionID: "d", Result: privacy.ResultBlock}, nil, "DENY"},
		"review":        {&privacy.Decision{DecisionID: "d", Result: privacy.ResultReviewRequired}, nil, "INDETERMINATE"},
		"indeterminate": {&privacy.Decision{DecisionID: "d", Result: privacy.ResultIndeterminate}, nil, "INDETERMINATE"},
		"outage":        {nil, privacy.ErrUnavailable, "INDETERMINATE"},
		"nil answer":    {nil, nil, "INDETERMINATE"},
	}
	for name, tc := range cases {
		if got := resolve(t, &fakeDecider{d: tc.d, err: tc.err}, boundIntent(), PermissionDecision{}).Permission.Decision; got != tc.want {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}
	// Even an error that wraps the sentinel is an outage, never permission.
	if got := resolve(t, &fakeDecider{err: errors.New("boom")}, boundIntent(), PermissionDecision{Decision: "PERMIT"}).Permission.Decision; got != "INDETERMINATE" {
		t.Fatalf("an unreachable service must not fall back to the caller's PERMIT: %s", got)
	}
}

func TestResolvePrivacy_RestrictionsOnlyWhenEnforceable(t *testing.T) {
	ok := &fakeDecider{d: &privacy.Decision{DecisionID: "d", Result: privacy.ResultRestrict, Constraints: []privacy.Constraint{{Type: "no_sms"}, {Type: "NO_PUSH"}}}}
	r := resolve(t, ok, boundIntent(), PermissionDecision{})
	if r.Permission.Decision != "RESTRICT" || len(r.Permission.Restrictions) != 2 || r.Permission.Restrictions[0] != "NO_SMS" {
		t.Fatalf("channel exclusions are carried through: %+v", r.Permission)
	}
	for _, c := range []string{"MINIMISE", "NO_CARRIER_PIGEON", "REDACT_BODY"} {
		f := &fakeDecider{d: &privacy.Decision{DecisionID: "d", Result: privacy.ResultRestrict, Constraints: []privacy.Constraint{{Type: "NO_SMS"}, {Type: c}}}}
		if got := resolve(t, f, boundIntent(), PermissionDecision{}).Permission.Decision; got != "DENY" {
			t.Errorf("a duty this plane cannot enforce (%s) must refuse, got %s", c, got)
		}
	}
}

func TestResolvePrivacy_AnUnboundIntentIsNeverAskedAndNeverPermitted(t *testing.T) {
	f := &fakeDecider{d: &privacy.Decision{DecisionID: "d", Result: privacy.ResultPermit}}
	r := resolve(t, f, Intent{}, PermissionDecision{Decision: "PERMIT"})
	if r.Permission.Decision != "INDETERMINATE" || f.n != 0 {
		t.Fatalf("no binding means nothing to ask and no permission: %+v asked=%d", r, f.n)
	}
}

func TestResolvePrivacy_WithoutADeciderTheCallerValueStandsAsBefore(t *testing.T) {
	r := resolve(t, nil, Intent{}, PermissionDecision{Decision: "DENY", DecisionID: "x"})
	if r.Source != "CALLER" || r.Permission.Decision != "DENY" {
		t.Fatalf("%+v", r)
	}
	if r := resolve(t, nil, Intent{}, PermissionDecision{}); r.Note == "" {
		t.Fatal("the recorded absence of a decision must carry its note")
	}
}

func TestIntentInput_PrivacyBindingIsBothOrNeitherAndWellFormed(t *testing.T) {
	base := IntentInput{LegalEntityID: "le", IntentCode: "x", DisplayName: "X", DomainOwner: "d", PurposeClass: PurposeOperational,
		Sensitivity: "S0", Urgency: "U0", EvidenceClass: "E0", AllowedChannels: []string{ChannelEmail}}
	for name, mut := range map[string]func(*IntentInput){
		"activity only": func(i *IntentInput) { i.PrivacyActivityID = "a" },
		"purpose only":  func(i *IntentInput) { i.PrivacyPurposeID = "p" },
		"bad chars":     func(i *IntentInput) { i.PrivacyActivityID, i.PrivacyPurposeID = "a b", "p" },
	} {
		in := base
		mut(&in)
		in.normalize()
		if e := in.validate(); e == nil || e.Code != "invalid_privacy_binding" {
			t.Errorf("%s: want invalid_privacy_binding, got %v", name, e)
		}
	}
	in := base
	in.PrivacyActivityID, in.PrivacyPurposeID = " act-1 ", "purp.1"
	in.normalize()
	if e := in.validate(); e != nil {
		t.Fatalf("a well-formed binding is accepted: %v", e)
	}
	if i := in.toIntent(); i.PrivacyActivityID != "act-1" || i.PrivacyPurposeID != "purp.1" {
		t.Fatalf("the binding is carried onto the intent, trimmed: %+v", i)
	}
}
