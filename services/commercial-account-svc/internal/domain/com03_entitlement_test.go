package domain_test

import (
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

func i64(v int64) *int64 { return &v }

func basis(status domain.LifecycleStatus, caps map[string]domain.PlanCapability) domain.SubscriptionEntitlementBasis {
	return domain.SubscriptionEntitlementBasis{SubscriptionID: "csub_x", Status: status, Capabilities: caps}
}

var now3 = time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC)

// Negative path #39: nobody grants a capability outside the effective
// subscription by fabricating a row — the decision has no grant to write.
func TestEvaluateCapability_NoSubscriptionDeniesEverything(t *testing.T) {
	d := domain.EvaluateCapability("api_access", domain.SubscriptionEntitlementBasis{}, nil, nil, nil, now3)
	if d.Outcome != domain.OutcomeDeny {
		t.Fatalf("no subscription: %v", d.Outcome)
	}
}

// Negative path #12: payment/billing state never enters this decision —
// only lifecycle status and the bound capabilities do.
func TestEvaluateCapability_ActiveGrantsWhatTheSubscriptionIncludes(t *testing.T) {
	caps := map[string]domain.PlanCapability{
		"api_access": {CapabilityKey: "api_access", LimitValue: nil},
		"api_calls":  {CapabilityKey: "api_calls", LimitValue: i64(1000), LimitUnit: sp("call")},
	}
	for _, status := range []domain.LifecycleStatus{domain.LifecycleActive, domain.LifecycleTrialing, domain.LifecycleCancelPending} {
		d := domain.EvaluateCapability("api_access", basis(status, caps), nil, nil, nil, now3)
		if d.Outcome != domain.OutcomeAllow {
			t.Errorf("%s: unlimited capability = %v, want ALLOW", status, d.Outcome)
		}
		d = domain.EvaluateCapability("api_calls", basis(status, caps), nil, nil, nil, now3)
		if d.Outcome != domain.OutcomeAllowWithLimit || d.LimitValue == nil || *d.LimitValue != 1000 {
			t.Errorf("%s: limited capability = %v limit=%v", status, d.Outcome, d.LimitValue)
		}
	}
	d := domain.EvaluateCapability("not_included", basis(domain.LifecycleActive, caps), nil, nil, nil, now3)
	if d.Outcome != domain.OutcomeDeny {
		t.Fatalf("a capability absent from the plan: %v, want DENY", d.Outcome)
	}
}

func TestEvaluateCapability_PendingIsDenied(t *testing.T) {
	caps := map[string]domain.PlanCapability{"api_access": {CapabilityKey: "api_access"}}
	d := domain.EvaluateCapability("api_access", basis(domain.LifecyclePending, caps), nil, nil, nil, now3)
	if d.Outcome != domain.OutcomeDeny {
		t.Fatalf("a pending (unactivated) subscription granted access: %v", d.Outcome)
	}
}

func TestEvaluateCapability_RequestedQuantityOverLimitDenies(t *testing.T) {
	caps := map[string]domain.PlanCapability{"api_calls": {CapabilityKey: "api_calls", LimitValue: i64(1000)}}
	under, over := i64(500), i64(1500)
	if d := domain.EvaluateCapability("api_calls", basis(domain.LifecycleActive, caps), nil, nil, under, now3); d.Outcome != domain.OutcomeAllowWithLimit {
		t.Fatalf("under the limit: %v", d.Outcome)
	}
	if d := domain.EvaluateCapability("api_calls", basis(domain.LifecycleActive, caps), nil, nil, over, now3); d.Outcome != domain.OutcomeDeny {
		t.Fatalf("over the limit: %v, want DENY", d.Outcome)
	}
}

// Negative path #13: payment past due with a grace policy returns the
// policy-defined restricted entitlement, not an arbitrary shutdown.
func TestEvaluateCapability_EndedSubscriptionFollowsThePublishedPolicy(t *testing.T) {
	endedAt := now3.Add(-10 * 24 * time.Hour)
	b := domain.SubscriptionEntitlementBasis{SubscriptionID: "csub_x", Status: domain.LifecycleCanceled, EndedAt: &endedAt}

	if d := domain.EvaluateCapability("api_access", b, nil, nil, nil, now3); d.Outcome != domain.OutcomeDeny {
		t.Fatalf("no published policy: %v, want fail-closed DENY", d.Outcome)
	}
	readOnly := &domain.EntitlementPolicyVersion{PolicyVersion: 1, EndedOutcome: "READ_ONLY", EndedReadOnlyDays: 14, EffectiveFrom: now3.Add(-30 * 24 * time.Hour)}
	if d := domain.EvaluateCapability("api_access", b, nil, readOnly, nil, now3); d.Outcome != domain.OutcomeReadOnly || *d.PolicyVersion != 1 {
		t.Fatalf("within the grace window: %v policy=%v", d.Outcome, d.PolicyVersion)
	}
	past := now3.Add(20 * 24 * time.Hour)
	if d := domain.EvaluateCapability("api_access", b, nil, readOnly, nil, past); d.Outcome != domain.OutcomeDeny {
		t.Fatalf("after the grace window: %v, want DENY", d.Outcome)
	}
	deny := &domain.EntitlementPolicyVersion{PolicyVersion: 2, EndedOutcome: "DENY", EffectiveFrom: now3}
	if d := domain.EvaluateCapability("api_access", b, nil, deny, nil, now3); d.Outcome != domain.OutcomeDeny {
		t.Fatalf("a DENY policy: %v", d.Outcome)
	}
}

// Negative path #08/#09: nothing applies before its effective time, and
// nothing survives it — but that is a property of the caller supplying the
// bound-at-now basis; EvaluateCapability itself just answers for the given
// now, which this proves by giving it two different bases for two different
// instants of the same subscription.
func TestEvaluateCapability_DecidesForTheGivenInstantOnly(t *testing.T) {
	before := basis(domain.LifecycleActive, map[string]domain.PlanCapability{"old_feature": {CapabilityKey: "old_feature"}})
	after := basis(domain.LifecycleActive, map[string]domain.PlanCapability{"new_feature": {CapabilityKey: "new_feature"}})
	if d := domain.EvaluateCapability("new_feature", before, nil, nil, nil, now3); d.Outcome != domain.OutcomeDeny {
		t.Fatalf("a not-yet-effective upgrade's capability was granted early: %v", d.Outcome)
	}
	if d := domain.EvaluateCapability("old_feature", after, nil, nil, nil, now3); d.Outcome != domain.OutcomeDeny {
		t.Fatalf("a superseded plan's capability survived its own replacement: %v", d.Outcome)
	}
}

// A restriction can only push a decision toward Deny, never toward Allow —
// applying one to a DENY subscription must stay DENY.
func TestEvaluateCapability_RestrictionOnlyLowersNeverRaises(t *testing.T) {
	caps := map[string]domain.PlanCapability{"api_access": {CapabilityKey: "api_access"}}
	active := basis(domain.LifecycleActive, caps)
	ro := []domain.CommercialRestriction{{RestrictionID: "crst_1", Level: domain.RestrictionReadOnly, AppliedAt: now3.Add(-time.Hour)}}
	d := domain.EvaluateCapability("api_access", active, ro, nil, nil, now3)
	if d.Outcome != domain.OutcomeReadOnly || d.AppliedRestrictionID == nil || *d.AppliedRestrictionID != "crst_1" {
		t.Fatalf("read-only restriction on an allowed capability: %v applied=%v", d.Outcome, d.AppliedRestrictionID)
	}

	restricted := []domain.CommercialRestriction{{RestrictionID: "crst_2", Level: domain.RestrictionRestricted, AppliedAt: now3.Add(-time.Hour)}}
	noSub := domain.EvaluateCapability("api_access", domain.SubscriptionEntitlementBasis{}, restricted, nil, nil, now3)
	if noSub.Outcome != domain.OutcomeDeny {
		t.Fatalf("a restriction upgraded a denied capability: %v", noSub.Outcome)
	}

	// Several active restrictions: the strictest wins.
	both := []domain.CommercialRestriction{
		{RestrictionID: "crst_ro", Level: domain.RestrictionReadOnly, AppliedAt: now3.Add(-2 * time.Hour)},
		{RestrictionID: "crst_hard", Level: domain.RestrictionRestricted, AppliedAt: now3.Add(-time.Hour)},
	}
	d = domain.EvaluateCapability("api_access", active, both, nil, nil, now3)
	if d.Outcome != domain.OutcomeRestricted || *d.AppliedRestrictionID != "crst_hard" {
		t.Fatalf("strictest-wins: %v applied=%v", d.Outcome, d.AppliedRestrictionID)
	}

	// Not yet applied, and already lifted: neither is active.
	future := []domain.CommercialRestriction{{RestrictionID: "crst_future", Level: domain.RestrictionRestricted, AppliedAt: now3.Add(time.Hour)}}
	if d := domain.EvaluateCapability("api_access", active, future, nil, nil, now3); d.Outcome != domain.OutcomeAllow {
		t.Fatalf("a not-yet-applied restriction was already in effect: %v", d.Outcome)
	}
	lifted := time.Time(now3.Add(-time.Minute))
	past := []domain.CommercialRestriction{{RestrictionID: "crst_lifted", Level: domain.RestrictionRestricted, AppliedAt: now3.Add(-time.Hour), LiftedAt: &lifted}}
	if d := domain.EvaluateCapability("api_access", active, past, nil, nil, now3); d.Outcome != domain.OutcomeAllow {
		t.Fatalf("a lifted restriction still applied: %v", d.Outcome)
	}
}

func TestEffectiveEntitlements_CoversEveryBoundCapability(t *testing.T) {
	caps := map[string]domain.PlanCapability{
		"api_access": {CapabilityKey: "api_access"},
		"api_calls":  {CapabilityKey: "api_calls", LimitValue: i64(100)},
	}
	ds := domain.EffectiveEntitlements(basis(domain.LifecycleActive, caps), nil, nil, now3)
	if len(ds) != 2 {
		t.Fatalf("%d decisions, want one per bound capability", len(ds))
	}
	empty := domain.EffectiveEntitlements(domain.SubscriptionEntitlementBasis{}, nil, nil, now3)
	if len(empty) != 1 || empty[0].Outcome != domain.OutcomeDeny {
		t.Fatalf("no subscription must still explain itself, not return an empty unexplained list: %+v", empty)
	}
}
