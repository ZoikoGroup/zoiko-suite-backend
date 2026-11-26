package store_test

import (
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// A8 (SPEC_DEVIATIONS.md): registerLateEvent needs a target OPEN statement to
// exist in the SAME transaction that records a late event. Before this fix,
// two real cases rolled back that whole transaction -- losing the late event
// itself, not just failing to carry its quantity forward:
//   - the origin term's own statement is FROZEN (not yet certified), so
//     there is nothing OPEN in that window to route into;
//   - the subscription has no term covering "now" at all.
// pending_late_adjustments (migration 000025) makes both durable instead.

// Same-term frozen statement: proves the late event is never lost, and
// finds the REAL re-attachment moment. Certifying the frozen origin alone
// does not help -- it creates nothing OPEN for that same window. Only a
// later term opening does, because the worker re-resolves a target at its
// own "now" on every pass, not the original observedAt.
func TestUsage_PendingLateAdjustment_SameTermFrozen_AttachesOnceANewTermOpens(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	term1 := sub.CurrentTerm.TermNo

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12))
	if _, err := f.s.CloseUsageWindow(f.ctx, sub.SubscriptionID, term1, "api.calls", publisher, day(20),
		f.claim(publisher, "close", sub.SubscriptionID+"api.calls")); err != nil {
		t.Fatalf("close window: %v", err)
	}
	frozen, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term1, "api.calls")
	if err != nil || frozen.Status != domain.StatementFrozen {
		t.Fatalf("setup: statement not FROZEN: %+v (err=%v)", frozen, err)
	}

	// Observed while term1 is still the current term and its statement is
	// FROZEN: pre-A8, findOrCreateOpenStatement would return
	// ErrStatementInvalidState and the WHOLE transaction -- including the
	// late usage_event_records row -- would roll back and be lost.
	observedAt := day(21)
	late, err := f.ingestAt(f.org, sub.SubscriptionID, "api.calls", 1, "evt-late", "3", day(15), observedAt)
	if err != nil {
		t.Fatalf("a late event while the origin is FROZEN must not be lost: %v", err)
	}
	if !late.Late || late.StatementID != nil {
		t.Fatalf("late event shape: %+v", late)
	}

	pending, err := f.s.GetPendingLateAdjustments(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected exactly one pending late adjustment: %+v (err=%v)", pending, err)
	}
	if pending[0].Status != domain.PendingLateAdjustmentPending || pending[0].SourceUsageEventID != "evt-late" ||
		pending[0].Quantity != "3" || pending[0].OriginStatementID != frozen.StatementID {
		t.Fatalf("pending row: %+v", pending[0])
	}

	// A drain while term1 is still current and still FROZEN finds no valid
	// target: it must record a retry, not an error, and must attach nothing.
	found, err := f.s.ProcessNextPendingLateAdjustment(f.ctx, day(22))
	if err != nil || !found {
		t.Fatalf("drain with no target yet: found=%v err=%v", found, err)
	}
	stillPending, err := f.s.GetPendingLateAdjustments(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(stillPending) != 1 || stillPending[0].Status != domain.PendingLateAdjustmentPending || stillPending[0].Attempts != 1 {
		t.Fatalf("pending row after a no-target drain: %+v (err=%v)", stillPending, err)
	}

	// Certifying the FROZEN origin alone still does not help: certification
	// does not open anything new for the SAME window.
	if _, err := f.s.CertifyUsageStatement(f.ctx, frozen.StatementID, publisher, day(23),
		f.claim(publisher, "certify", frozen.StatementID)); err != nil {
		t.Fatalf("certify origin: %v", err)
	}
	if found, err := f.s.ProcessNextPendingLateAdjustment(f.ctx, day(24)); err != nil || !found {
		t.Fatalf("drain after certifying origin alone: found=%v err=%v", found, err)
	}
	stillPending2, err := f.s.GetPendingLateAdjustments(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(stillPending2) != 1 || stillPending2[0].Status != domain.PendingLateAdjustmentPending || stillPending2[0].Attempts != 2 {
		t.Fatalf("pending row after certifying origin alone: %+v (err=%v)", stillPending2, err)
	}

	// The real re-attachment moment: a later term opens.
	renewAt := sub.CurrentTerm.EndsAt.Add(time.Hour)
	sub2 := f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, renewAt), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	term2 := sub2.CurrentTerm.TermNo

	found, err = f.s.ProcessNextPendingLateAdjustment(f.ctx, renewAt.Add(time.Hour))
	if err != nil || !found {
		t.Fatalf("drain once term2 is open: found=%v err=%v", found, err)
	}
	attached, err := f.s.GetPendingLateAdjustments(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(attached) != 1 || attached[0].Status != domain.PendingLateAdjustmentAttached || attached[0].AttachedAdjustmentID == nil {
		t.Fatalf("pending row after attach: %+v (err=%v)", attached, err)
	}

	target, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term2, "api.calls")
	if err != nil || target.EventCount != 1 {
		t.Fatalf("the late quantity was not carried into term2: %+v (err=%v)", target, err)
	}
	certifiedTarget := f.closeAndCertify(sub.SubscriptionID, term2, "api.calls", sub2.CurrentTerm.EndsAt.Add(time.Hour))
	if certifiedTarget.Status != domain.StatementAdjusted || certifiedTarget.TotalQuantity != "3.0000" {
		t.Fatalf("term2 certified total must include the attached late quantity: %+v", certifiedTarget)
	}

	// A second drain finds nothing more to do: the item is ATTACHED, final.
	if found, err := f.s.ProcessNextPendingLateAdjustment(f.ctx, renewAt.Add(2*time.Hour)); err != nil || found {
		t.Fatalf("a second drain found another item: found=%v err=%v", found, err)
	}

	// The original FROZEN-then-certified statement is unaffected throughout.
	afterOrigin, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term1, "api.calls")
	if err != nil || afterOrigin.TotalQuantity != "5.0000" || afterOrigin.Status != domain.StatementCertified {
		t.Fatalf("origin statement changed: %+v (err=%v)", afterOrigin, err)
	}
}

// Post-final-term event: the subscription has no term covering "now" at
// all (no renewal; the term it belongs to has simply ended). The spec gives
// no expiry for a late correction (§4.4, §9 negative path #18 says only
// "create prospective adjustment"), so this stays PENDING indefinitely
// rather than being discarded or turned into a terminal rejection: repeated
// drains keep retrying -- recording the cause as evidence -- never erroring
// the caller and never losing the fact.
func TestUsage_PendingLateAdjustment_PostFinalTerm_StaysPendingIndefinitely(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: false})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	term1 := sub.CurrentTerm.TermNo

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "10", day(12))
	origin := f.closeAndCertify(sub.SubscriptionID, term1, "api.calls", day(20))

	// No renewal: once term1 ends there is no term covering "now" at all.
	observedAt := sub.CurrentTerm.EndsAt.Add(24 * time.Hour)
	late, err := f.ingestAt(f.org, sub.SubscriptionID, "api.calls", 1, "evt-late", "3", day(15), observedAt)
	if err != nil {
		t.Fatalf("a late event after the subscription's final term must not be lost: %v", err)
	}
	if !late.Late || late.StatementID != nil {
		t.Fatalf("late event shape: %+v", late)
	}

	pending, err := f.s.GetPendingLateAdjustments(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(pending) != 1 || pending[0].Status != domain.PendingLateAdjustmentPending {
		t.Fatalf("pending row: %+v (err=%v)", pending, err)
	}

	// Repeated drains never error and never attach -- there is still nowhere
	// for the quantity to go -- but the fact is never lost or parked FAILED:
	// this queue has no terminal/expired state (see
	// com04_pending_late_adjustments.go's package comment).
	for i, at := range []time.Time{observedAt.Add(time.Hour), observedAt.Add(2 * time.Hour), observedAt.Add(3 * time.Hour)} {
		found, err := f.s.ProcessNextPendingLateAdjustment(f.ctx, at)
		if err != nil || !found {
			t.Fatalf("drain %d with no term at all: found=%v err=%v", i, found, err)
		}
	}
	stillPending, err := f.s.GetPendingLateAdjustments(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(stillPending) != 1 || stillPending[0].Status != domain.PendingLateAdjustmentPending || stillPending[0].Attempts != 3 {
		t.Fatalf("pending row after repeated drains: %+v (err=%v)", stillPending, err)
	}
	if stillPending[0].LastError == nil {
		t.Fatalf("expected the no-target cause to be recorded as evidence: %+v", stillPending[0])
	}

	// The original certified statement is untouched throughout.
	afterOrigin, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term1, "api.calls")
	if err != nil || afterOrigin.TotalQuantity != origin.TotalQuantity {
		t.Fatalf("origin statement changed: %+v (err=%v)", afterOrigin, err)
	}
}
