package store_test

import (
	"context"
	"errors"
	"strconv"
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

// A6: a replayed PublishEntitlementPolicy under the same idempotency key
// must return the exact original command's own version — not whichever
// version happens to be effective "now" at replay time, which genuinely
// differs here: the original call published a future-dated DENY policy that
// is not yet effective, so "what's effective now" is still the earlier
// READ_ONLY baseline. A pre-fix replay would have returned that baseline,
// not the DENY version the original call actually created.
func TestEntitlement_PublishEntitlementPolicyReplayReturnsOriginalVersion(t *testing.T) {
	f := newSub(t)
	baseline := f.policy("READ_ONLY", 30, publisher)

	future := day(1000)
	p := &domain.EntitlementPolicyVersion{EndedOutcome: "DENY", EffectiveFrom: future, Reason: "future hard cutover",
		CreatedAt: day(5), CreatedByPrincipalID: publisher}
	claim := f.claim(publisher, "PublishEntitlementPolicy", "grace-policy")
	original, err := f.s.PublishEntitlementPolicy(f.ctx, p, claim)
	if err != nil {
		t.Fatalf("publish future policy: %v", err)
	}
	if original.PolicyVersion != baseline.PolicyVersion+1 || original.EndedOutcome != "DENY" {
		t.Fatalf("original publish: %+v", original)
	}

	// What's effective "now" (day(5), before the future policy's effective
	// date) is still the baseline — confirming a recompute-fresh replay
	// really would answer differently from the original command's result.
	effectiveNow, err := f.s.GetEntitlementPolicy(f.ctx, day(5))
	if err != nil || effectiveNow == nil || effectiveNow.PolicyVersion != baseline.PolicyVersion {
		t.Fatalf("effective-now baseline: %+v (err=%v)", effectiveNow, err)
	}

	// Retry under the identical claim (same key, same body hash): a second,
	// unrelated policy must NOT have been inserted (it is still would-be
	// version baseline+2 if a fresh one got created), and the replay error's
	// resource id must resolve back to the ORIGINAL future DENY version.
	p2 := &domain.EntitlementPolicyVersion{EndedOutcome: "DENY", EffectiveFrom: future, Reason: "future hard cutover",
		CreatedAt: day(5), CreatedByPrincipalID: publisher}
	_, err = f.s.PublishEntitlementPolicy(f.ctx, p2, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		t.Fatalf("retry under the same key did not replay: %v", err)
	}
	version, perr := strconv.Atoi(replay.ResourceID)
	if perr != nil {
		t.Fatalf("replay resource id %q is not the policy_version: %v", replay.ResourceID, perr)
	}
	got, err := f.s.GetEntitlementPolicyByVersion(f.ctx, version)
	if err != nil || got == nil {
		t.Fatalf("get replayed version: %+v (err=%v)", got, err)
	}
	if got.PolicyVersion != original.PolicyVersion || got.EndedOutcome != "DENY" || !got.EffectiveFrom.Equal(future) {
		t.Fatalf("replayed policy = %+v, want the original future DENY version %+v", got, original)
	}
	var count int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM entitlement_policy_versions`).Scan(&count); err != nil {
		t.Fatalf("count policy versions: %v", err)
	}
	if count != 2 {
		t.Fatalf("replay must not insert a second row: %d policy versions exist, want 2", count)
	}
}

// A6: a replayed CreateSnapshot under the same idempotency key must return
// the exact decisions the original call captured, even though the
// organization's genuinely effective entitlements have since changed (here,
// a restriction applied after the original snapshot). A recompute-fresh
// replay (the pre-fix behavior) would answer RESTRICTED, not the original
// ALLOW — exactly the divergence this proves matters.
func TestEntitlement_CreateSnapshotReplayReturnsOriginalResult(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, caps: []domain.PlanCapability{{CapabilityKey: "api_access"}}})
	activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	batchID1 := domain.NewCommercialID(domain.PrefixEntitlementSnapshotBatch)
	claim := f.claim(operator, "CreateSnapshot", batchID1)
	original, err := f.s.CreateSnapshot(f.ctx, f.org, operator, day(12), claim, batchID1)
	if err != nil || len(original) == 0 {
		t.Fatalf("create snapshot: %+v (err=%v)", original, err)
	}
	var originalOutcome domain.Outcome
	for _, d := range original {
		if d.CapabilityKey == "api_access" {
			originalOutcome = d.Outcome
		}
	}
	if originalOutcome != domain.OutcomeAllow {
		t.Fatalf("original outcome = %s, want ALLOW", originalOutcome)
	}

	// State genuinely changes between the original call and the replay.
	f.restrict(domain.RestrictionRestricted, operator, day(13))
	fresh, err := f.s.GetEffectiveEntitlements(f.ctxOrg, day(14))
	if err != nil {
		t.Fatalf("get effective (fresh): %v", err)
	}
	var freshOutcome domain.Outcome
	for _, d := range fresh {
		if d.CapabilityKey == "api_access" {
			freshOutcome = d.Outcome
		}
	}
	if freshOutcome != domain.OutcomeRestricted {
		t.Fatalf("a recompute-fresh read after the restriction = %s, want RESTRICTED (so the two must genuinely differ)", freshOutcome)
	}

	// Retry under the identical claim: a different, unrelated local batch id
	// (simulating a second process retrying with no memory of the first)
	// must not matter — the replay must resolve back to the ORIGINAL batch.
	batchID2 := domain.NewCommercialID(domain.PrefixEntitlementSnapshotBatch)
	_, err = f.s.CreateSnapshot(f.ctx, f.org, operator, day(14), claim, batchID2)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		t.Fatalf("retry under the same key did not replay: %v", err)
	}
	if replay.ResourceID != batchID1 {
		t.Fatalf("replay resource id = %s, want the original batch id %s", replay.ResourceID, batchID1)
	}
	replayed, err := f.s.GetEntitlementSnapshotBatch(f.ctxOrg, f.org, replay.ResourceID)
	if err != nil || len(replayed) == 0 {
		t.Fatalf("get replayed batch: %+v (err=%v)", replayed, err)
	}
	var replayedOutcome domain.Outcome
	for _, d := range replayed {
		if d.CapabilityKey == "api_access" {
			replayedOutcome = d.Outcome
		}
	}
	if replayedOutcome != domain.OutcomeAllow {
		t.Fatalf("replayed outcome = %s, want the ORIGINAL ALLOW, not the current RESTRICTED state", replayedOutcome)
	}
}
