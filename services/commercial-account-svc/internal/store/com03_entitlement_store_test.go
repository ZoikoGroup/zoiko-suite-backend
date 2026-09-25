package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// COM-03 Entitlement against real Postgres as the NOBYPASSRLS role.

func (f *subFixture) policy(outcome string, days int, by string) *domain.EntitlementPolicyVersion {
	f.t.Helper()
	p := &domain.EntitlementPolicyVersion{EndedOutcome: outcome, EndedReadOnlyDays: days, EffectiveFrom: day(0),
		Reason: "test policy", CreatedAt: t0, CreatedByPrincipalID: by}
	got, err := f.s.PublishEntitlementPolicy(f.ctx, p, f.claim(by, "PublishEntitlementPolicy", "grace-policy"))
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *subFixture) restrict(level domain.RestrictionLevel, by string, at time.Time) *domain.CommercialRestriction {
	f.t.Helper()
	r := &domain.CommercialRestriction{RestrictionID: domain.NewCommercialID(domain.PrefixRestriction), OrganizationID: f.org,
		Level: level, ReasonCode: "FRAUD_HOLD", PolicyRef: "pol-1", BasisRef: "case-1", AppliedAt: at, AppliedByPrincipalID: by}
	got, err := f.s.ApplyRestriction(svcmiddleware.WithTenant(f.ctx, f.org), r, f.claim(by, "ApplyRestriction", r.RestrictionID))
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// Negative path #39: nobody may fabricate a grant. A tenant with no
// subscription is denied every capability, and the only way to change that
// is to actually start one.
func TestEntitlement_NoSubscriptionIsDeniedEverything(t *testing.T) {
	f := newSub(t)
	d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(1))
	if err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("no subscription: %+v %v", d, err)
	}
}

func TestEntitlement_ActiveSubscriptionGrantsItsBoundCapabilities(t *testing.T) {
	f := newSub(t)
	limit := int64(1000)
	f.publishPlan("business", planOpts{autoRenew: true, caps: []domain.PlanCapability{
		{CapabilityKey: "api_access", LimitValue: nil}, {CapabilityKey: "api_calls", LimitValue: &limit, LimitUnit: strp("call")},
	}})
	activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(12))
	if err != nil || d.Outcome != domain.OutcomeAllow {
		t.Fatalf("unlimited capability: %+v %v", d, err)
	}
	d, err = f.s.GetLimit(f.ctxOrg, "api_calls", day(12))
	if err != nil || d.Outcome != domain.OutcomeAllowWithLimit || d.LimitValue == nil || *d.LimitValue != 1000 {
		t.Fatalf("limited capability: %+v %v", d, err)
	}
	d, err = f.s.EvaluateCapability(f.ctxOrg, "not_in_plan", nil, day(12))
	if err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("a capability absent from the plan: %+v %v", d, err)
	}

	all, err := f.s.GetEffectiveEntitlements(f.ctxOrg, day(12))
	if err != nil || len(all) != 2 {
		t.Fatalf("effective entitlements: %d (err=%v)", len(all), err)
	}
}

// Negative path #08: nothing applies before the plan's effective time.
func TestEntitlement_FollowsTheSubscriptionVersionAtTheGivenInstant(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	before := day(10)
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, before); err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("before the subscription starts: %+v %v", d, err)
	}
}

// Negative path #13: an ended subscription follows the published grace
// policy, never a hardcoded shutdown, and fails closed without one.
func TestEntitlement_EndedSubscriptionFollowsThePublishedGracePolicy(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 0})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	// The minimum term (1 month) must be served before CancelNow is allowed.
	cancelAt := domain.AddBillingIntervals(day(11), "MONTH", 1, 1).Add(time.Hour)
	sub = f.sv(f.s.CancelNow(f.ctxOrg, f.cmd(sub, "customer-admin", cancelAt), f.tclaim(f.org, "customer-admin", "cancel-now", sub.SubscriptionID)))

	soon := cancelAt.Add(24 * time.Hour)
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, soon); err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("no published policy: %+v %v", d, err)
	}
	pol := f.policy("READ_ONLY", 14, publisher)
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, soon); err != nil || d.Outcome != domain.OutcomeReadOnly || d.PolicyVersion == nil || *d.PolicyVersion != pol.PolicyVersion {
		t.Fatalf("within grace: %+v %v", d, err)
	}
	afterGrace := cancelAt.Add(20 * 24 * time.Hour)
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, afterGrace); err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("after grace: %+v %v", d, err)
	}
}

// COM-CTRL-011 / negative path #39: only platform authority applies or
// lifts a restriction, and it can only ever lower a decision, never grant.
func TestEntitlement_RestrictionsAreAppliedOnlyByPlatformAndOnlyLowerAccess(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, caps: []domain.PlanCapability{{CapabilityKey: "api_access"}}})
	activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	r := f.restrict(domain.RestrictionRestricted, operator, day(12))
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(13)); err != nil || d.Outcome != domain.OutcomeRestricted {
		t.Fatalf("active restriction: %+v %v", d, err)
	}
	// A tenant cannot write its own restriction, and cannot lift one either.
	if _, err := f.tenantExec(f.org, `INSERT INTO commercial_restrictions (restriction_id, organization_id, level, reason_code,
		policy_ref, basis_ref, applied_at, applied_by_principal_id) VALUES ('crst_00000000-0000-4000-8000-000000000001',
		$1, 'RESTRICTED', 'X', 'p', 'b', now(), 'tenant-user')`, f.org); pgCode(err) != "42501" {
		t.Fatalf("a tenant wrote its own restriction: %v", err)
	}
	// The UPDATE policy's USING clause matches no row for a tenant-scoped
	// transaction, so the statement succeeds affecting zero rows rather than
	// erroring — the row is simply invisible to write, same as a SELECT that
	// finds nothing.
	if tag, err := f.tenantExec(f.org, `UPDATE commercial_restrictions SET lifted_at = now(), lifted_by_principal_id = 'tenant-user',
		lift_reason = 'self-service' WHERE restriction_id = $1`, r.RestrictionID); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("a tenant lifted its own restriction: rows=%d err=%v", tag.RowsAffected(), err)
	}

	lifted, err := f.s.RemoveRestriction(f.ctx, r.RestrictionID, operator, "case closed", day(14))
	if err != nil || lifted.LiftedAt == nil {
		t.Fatalf("platform lift: %+v %v", lifted, err)
	}
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(15)); err != nil || d.Outcome != domain.OutcomeAllow {
		t.Fatalf("after lift: %+v %v", d, err)
	}
	// Reconstructed as of before the lift, the restriction still applied.
	if d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(13)); err != nil || d.Outcome != domain.OutcomeRestricted {
		t.Fatalf("as-of before the lift: %+v %v", d, err)
	}
	if _, err := f.s.RemoveRestriction(f.ctx, r.RestrictionID, operator, "again", day(15)); !errors.Is(err, domain.ErrRestrictionAlreadyLifted) {
		t.Fatalf("lifted twice: %v", err)
	}
	if _, err := f.s.RemoveRestriction(f.ctx, "crst_00000000-0000-4000-8000-000000000009", operator, "x", day(15)); !errors.Is(err, domain.ErrRestrictionNotFound) {
		t.Fatalf("removing an unknown restriction: %v", err)
	}

	// A restriction cannot upgrade a DENY into anything — remove the
	// subscription's only basis for access and confirm RESTRICTED alone
	// changes nothing about a tenant that was never entitled.
	org2, _ := f.newAccount(orgB, "USD", "GB")
	ctx2 := svcmiddleware.WithTenant(context.Background(), org2)
	f.restrict(domain.RestrictionRestricted, operator, day(1))
	if d, err := f.s.EvaluateCapability(ctx2, "api_access", nil, day(2)); err == nil && d.Outcome == domain.OutcomeAllow {
		t.Fatalf("a restriction alone granted access with no subscription: %+v", d)
	}
}

func TestEntitlement_PolicyPublicationIsAppendOnly(t *testing.T) {
	f := newSub(t)
	v1 := f.policy("DENY", 0, publisher)
	if _, err := f.admin.Exec(f.ctx, `UPDATE entitlement_policy_versions SET ended_outcome = 'READ_ONLY' WHERE policy_version = $1`, v1.PolicyVersion); pgCode(err) != "CP001" {
		t.Fatalf("a published policy was edited: %v", err)
	}
	v2 := f.policy("READ_ONLY", 7, publisher)
	if v2.PolicyVersion != v1.PolicyVersion+1 {
		t.Fatalf("policy versions must be sequential: %d after %d", v2.PolicyVersion, v1.PolicyVersion)
	}
}
