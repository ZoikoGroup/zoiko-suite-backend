package store_test

import (
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// COM-03 gap-remediation wave against real Postgres, following a skeptical
// re-audit that found EvaluateCapability never consulted actual usage, no
// automatic entitlement invalidation on subscription change, no dunning ->
// restriction wiring, and no persisted EntitlementSnapshot.

// B1: a metered capability's limit is checked against cumulative
// consumption, including usage that is still in an OPEN (uncertified)
// statement — not just a single request in isolation.
func TestGapFix_QuotaConsultationDeniesOverCumulativeUsage(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	meterKey := "api.calls"
	limit := int64(1000)
	f.publishPlan("business", planOpts{autoRenew: true, caps: []domain.PlanCapability{
		{CapabilityKey: "api_calls", LimitValue: &limit, LimitUnit: strp("call"), MeterKey: &meterKey, MeterVersion: intp(1)},
	}})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "900", day(12))

	small := int64(50)
	d, err := f.s.EvaluateCapability(f.ctxOrg, "api_calls", &small, day(13))
	if err != nil || d.Outcome != domain.OutcomeAllowWithLimit {
		t.Fatalf("900 consumed + 50 requested (still under 1000): %+v (err=%v)", d, err)
	}
	tooMuch := int64(150)
	d, err = f.s.EvaluateCapability(f.ctxOrg, "api_calls", &tooMuch, day(13))
	if err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("900 consumed + 150 requested (over 1000) was not denied even though 150 alone is under the static limit: %+v (err=%v)", d, err)
	}

	// Certify the window at exactly the limit; consumption must still be
	// read correctly from the now-CERTIFIED statement's authoritative
	// total_quantity, and a bare eligibility check against a fully
	// exhausted quota must deny.
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-2", "100", day(13))
	f.closeAndCertify(sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", day(14))
	d, err = f.s.EvaluateCapability(f.ctxOrg, "api_calls", nil, day(14))
	if err != nil || d.Outcome != domain.OutcomeDeny {
		t.Fatalf("quota fully exhausted (1000/1000) after certification, bare eligibility check: %+v (err=%v)", d, err)
	}
}

// B2: a boundary-driven subscription change automatically invalidates
// entitlement — an EntitlementSnapshot is recorded and the
// entitlement_snapshot.changed event fires without any explicit
// :recompute call.
func TestGapFix_BoundaryDrivenChangeAutoInvalidatesEntitlement(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "49.00", caps: []domain.PlanCapability{
		{CapabilityKey: "api_access", LimitValue: nil},
	}})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00", caps: []domain.PlanCapability{
		{CapabilityKey: "api_access", LimitValue: nil},
	}})
	f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationDailyHalfEven)
	sub := f.activeOn("business", nil)

	q, err := f.preview(sub, planChange("enterprise"), midTerm)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := f.change(sub, planChange("enterprise"), q.QuoteSHA256, midTerm); err != nil {
		t.Fatalf("change: %v", err)
	}

	// The immediate change recomputes in the same transaction as the
	// change itself — no separate :recompute call was made.
	history, err := f.s.GetEntitlementHistory(f.ctxOrg, f.org, "api_access", t0, midTerm.Add(time.Hour))
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("no entitlement snapshot was recorded automatically after an immediate subscription change")
	}
}

// B3: a dunning case escalating to RESTRICTED automatically applies a real
// CommercialRestriction — reflected in EvaluateCapability without any
// separate, manual ApplyRestriction call — and StopDunning lifts it again.
func TestGapFix_DunningEscalationAutoAppliesAndLiftsRestriction(t *testing.T) {
	f := newSub(t)
	f.mustPublishDunningPolicy(1, 0, 5, 10, 15)
	f.publishPlan("business", planOpts{autoRenew: true, base: "20.00", caps: []domain.PlanCapability{
		{CapabilityKey: "api_access", LimitValue: nil},
	}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	d, err := f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(30))
	if err != nil || d.Outcome != domain.OutcomeAllow {
		t.Fatalf("before dunning: %+v (err=%v)", d, err)
	}

	dc, err := f.startDunning(inv.InvoiceID)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	dc, err = f.s.AdvanceDunning(f.ctx, dc.CaseID, publisher, day(31), f.claim(publisher, "advance-1", dc.CaseID))
	if err != nil || dc.Status != domain.DunningNotice2 {
		t.Fatalf("advance to NOTICE_2: %+v (err=%v)", dc, err)
	}
	dc, err = f.s.AdvanceDunning(f.ctx, dc.CaseID, publisher, day(32), f.claim(publisher, "advance-2", dc.CaseID))
	if err != nil || dc.Status != domain.DunningRestricted || dc.AppliedRestrictionID == nil {
		t.Fatalf("advance to RESTRICTED must apply a restriction automatically: %+v (err=%v)", dc, err)
	}

	// No manual ApplyRestriction call was made; the decision must already
	// reflect it.
	d, err = f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(32))
	if err != nil || d.Outcome != domain.OutcomeRestricted {
		t.Fatalf("after automatic dunning restriction: %+v (err=%v)", d, err)
	}

	if _, err := f.s.StopDunning(f.ctx, dc.CaseID, publisher, "customer paid", day(33), f.claim(publisher, "stop", dc.CaseID)); err != nil {
		t.Fatalf("stop dunning: %v", err)
	}
	d, err = f.s.EvaluateCapability(f.ctxOrg, "api_access", nil, day(33))
	if err != nil || d.Outcome != domain.OutcomeAllow {
		t.Fatalf("after stopping dunning, the restriction must be lifted: %+v (err=%v)", d, err)
	}
}

// B4: CreateSnapshot is the explicit evidence-capture command, and
// entitlement_snapshots rejects a raw mutation at the database.
func TestGapFix_CreateSnapshotAndImmutability(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, caps: []domain.PlanCapability{
		{CapabilityKey: "api_access", LimitValue: nil},
	}})
	activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	ds, err := f.s.CreateSnapshot(f.ctx, f.org, publisher, day(12), f.claim(publisher, "CreateSnapshot", f.org))
	if err != nil || len(ds) == 0 {
		t.Fatalf("create snapshot: %+v (err=%v)", ds, err)
	}
	history, err := f.s.GetEntitlementHistory(f.ctxOrg, f.org, "api_access", day(11), day(13))
	if err != nil || len(history) == 0 {
		t.Fatalf("get history after explicit snapshot: %d (err=%v)", len(history), err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE entitlement_snapshots SET reason = 'tampered' WHERE snapshot_id = $1`,
		history[0].SnapshotID); err == nil {
		t.Fatal("a raw UPDATE against an entitlement snapshot was not rejected")
	}
}
