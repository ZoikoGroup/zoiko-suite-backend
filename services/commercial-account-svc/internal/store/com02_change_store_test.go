package store_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// COM-02 part 2b: transition rules, plan/quantity/add-on changes and
// proration, against real Postgres as the NOBYPASSRLS role.

func (f *subFixture) rule(from, to, timing, method string) *domain.PlanTransitionRule {
	f.t.Helper()
	r := &domain.PlanTransitionRule{RuleID: domain.NewCommercialID(domain.PrefixTransitionRule), FromProductCode: from,
		ToProductCode: to, Timing: timing, ProrationMethod: method, CreatedAt: t0, CreatedByPrincipalID: publisher}
	got, err := f.s.CreateTransitionRule(f.ctx, r, f.claim(publisher, "CreateTransitionRule", r.RuleID))
	if err != nil {
		f.t.Fatalf("create rule %s->%s: %v", from, to, err)
	}
	return got
}

// activeOn starts and activates a subscription on code at day 11 (12 Jan
// 09:00), giving a first term of exactly 31 days.
func (f *subFixture) activeOn(code string, quantities map[string]string) *domain.SubscriptionView {
	f.t.Helper()
	p := f.startParams(f.account, code, day(11))
	if quantities != nil {
		p.Quantities = quantities
	}
	return activateNow(f, f.sv(f.start(p)), day(11))
}

func (f *subFixture) preview(v *domain.SubscriptionView, req domain.ChangeRequest, at time.Time) (*domain.ChangeQuote, error) {
	return f.s.PreviewChange(f.ctxOrg, v.SubscriptionID, req, at)
}

func (f *subFixture) change(v *domain.SubscriptionView, req domain.ChangeRequest, quote string, at time.Time) (*domain.SubscriptionView, error) {
	return f.s.RequestChange(f.ctxOrg, f.cmd(v, "customer-admin", at), req, quote, f.tclaim(f.org, "customer-admin", "change", v.SubscriptionID))
}

func planChange(code string) domain.ChangeRequest {
	return domain.ChangeRequest{Kind: domain.ChangeKindPlan, PlanProductCode: code, PlanQuantities: map[string]string{},
		PlanAcceptedTermsSHA256: termsHash}
}

// midTerm is 22 Jan 11:30: 20 whole days remain of the 31-day term; the
// partial day belongs to the old plan.
var midTerm = day(21).Add(150 * time.Minute)

// COM-CTRL-009 / §4.2: an immediate upgrade is prorated from the two bound
// price versions, and the stored evidence recomputes to the same amounts.
func TestChange_ImmediateUpgradeIsProratedAndReproducible(t *testing.T) {
	f := newSub(t)
	business := f.publishPlan("business", planOpts{autoRenew: true})
	enterprise := f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationDailyHalfEven)
	sub := f.activeOn("business", nil)

	q, err := f.preview(sub, planChange("enterprise"), midTerm)
	if err != nil {
		t.Fatal(err)
	}
	if q.Timing != domain.TimingImmediate || q.Proration == nil || q.Proration.DaysInTerm != 31 || q.Proration.DaysRemaining != 20 ||
		q.Proration.Credit != "31.61" || q.Proration.Charge != "63.87" || q.Proration.Net != "32.26" {
		t.Fatalf("quote: %+v proration=%+v", q, q.Proration)
	}

	sub = f.sv(f.change(sub, planChange("enterprise"), q.QuoteSHA256, midTerm))
	if sub.Effective.Items[0].PriceVersionID != enterprise.PriceVersionID || sub.Effective.ChangeType != domain.ChangePlanChanged {
		t.Fatalf("after the upgrade: %+v", sub.Effective)
	}
	// Negative path #08: nothing of the new plan applies before its
	// effective time.
	before, _ := f.s.GetEffectiveVersion(f.ctxOrg, sub.SubscriptionID, midTerm.Add(-time.Minute))
	if before.Items[0].PriceVersionID != business.PriceVersionID {
		t.Fatalf("the upgrade applied before its effective time")
	}

	changes, err := f.s.GetChanges(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(changes) != 1 {
		t.Fatalf("change evidence: %d rows (err=%v)", len(changes), err)
	}
	ch := changes[0]
	if ch.OldPeriodCharge != "49.00000000" || ch.NewPeriodCharge != "99.00000000" || ch.Proration == nil ||
		ch.Proration.Net != "32.26" || ch.QuoteSHA256 != q.QuoteSHA256 || ch.FromPriceVersionID != business.PriceVersionID {
		t.Fatalf("evidence: %+v", ch)
	}
	oldC, _ := new(big.Rat).SetString(ch.OldPeriodCharge)
	newC, _ := new(big.Rat).SetString(ch.NewPeriodCharge)
	re := domain.Prorate(oldC, newC, ch.TermStartsAt, ch.TermEndsAt, ch.EffectiveAt, 2)
	if re != *ch.Proration {
		t.Fatalf("the stored evidence does not recompute to the stored amounts: %+v vs %+v", re, *ch.Proration)
	}
	if n := f.outboxCount(sub.SubscriptionID, "subscription.changed"); n != 1 {
		t.Fatalf("subscription.changed events: %d", n)
	}
}

// A confirmation must match the quote the customer saw.
func TestChange_StaleQuoteIsRefused(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationDailyHalfEven)
	sub := f.activeOn("business", nil)

	q, err := f.preview(sub, planChange("enterprise"), midTerm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.change(sub, planChange("enterprise"), q.QuoteSHA256, midTerm.Add(24*time.Hour)); !errors.Is(err, domain.ErrQuoteChanged) {
		t.Fatalf("a day-old quote was accepted after the proration changed: %v", err)
	}
	history, _ := f.s.GetChangeHistory(f.ctxOrg, sub.SubscriptionID)
	for _, v := range history {
		if v.ChangeType == domain.ChangePlanChanged {
			t.Fatal("a refused change wrote a version")
		}
	}
}

// A downgrade at renewal changes nothing now, is not prorated, and prices
// the next term under the new plan.
func TestChange_DowngradeAtRenewalRepricesOnlyTheNextTerm(t *testing.T) {
	f := newSub(t)
	business := f.publishPlan("business", planOpts{autoRenew: true})
	enterprise := f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.rule("enterprise", "business", domain.TimingNextRenewal, domain.ProrationNone)
	sub := f.activeOn("enterprise", nil)
	termEnd := sub.CurrentTerm.EndsAt

	q, err := f.preview(sub, planChange("business"), midTerm)
	if err != nil {
		t.Fatal(err)
	}
	if q.Timing != domain.TimingNextRenewal || q.Proration != nil || !q.EffectiveAt.Equal(termEnd) {
		t.Fatalf("downgrade quote: %+v", q)
	}
	sub = f.sv(f.change(sub, planChange("business"), q.QuoteSHA256, midTerm))
	if sub.Effective.Items[0].PriceVersionID != enterprise.PriceVersionID || len(sub.Scheduled) != 1 {
		t.Fatalf("a renewal-time downgrade changed the current plan: %+v", sub.Effective.Items)
	}
	if _, err := f.s.ScheduleCancellation(f.ctxOrg, f.cmd(sub, "customer-admin", midTerm),
		f.tclaim(f.org, "customer-admin", "cancel", sub.SubscriptionID)); !errors.Is(err, domain.ErrChangeAlreadyScheduled) {
		t.Fatalf("a cancellation was stacked on a scheduled downgrade: %v", err)
	}

	sub = f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, termEnd), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	if sub.CurrentTerm.PriceVersionID != business.PriceVersionID || sub.Effective.Items[0].PriceVersionID != business.PriceVersionID {
		t.Fatalf("the renewed term must be priced under the downgraded plan: term=%s", sub.CurrentTerm.PriceVersionID)
	}
	changes, _ := f.s.GetChanges(f.ctxOrg, sub.SubscriptionID)
	if len(changes) != 1 || changes[0].Timing != domain.TimingNextRenewal || changes[0].Proration != nil {
		t.Fatalf("downgrade evidence: %+v", changes)
	}
}

func TestChange_RulesGateEveryChange(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	annual := f.publishPlan("annual", planOpts{autoRenew: true, base: "490.00", interval: "YEAR"})
	sub := f.activeOn("business", nil)

	if _, err := f.preview(sub, planChange("enterprise"), midTerm); !errors.Is(err, domain.ErrTransitionNotAllowed) {
		t.Fatalf("a change with no rule was quoted: %v", err)
	}

	f.rule("business", "annual", domain.TimingImmediate, domain.ProrationDailyHalfEven)
	if _, err := f.preview(sub, planChange("annual"), midTerm); !errors.Is(err, domain.ErrTimingNotAllowed) {
		t.Fatalf("an immediate change to a different billing interval was quoted: %v", err)
	}
	later := planChange("annual")
	later.ForceNextRenewal = true
	q, err := f.preview(sub, later, midTerm)
	if err != nil {
		t.Fatalf("the same change at the next renewal must be allowed: %v", err)
	}
	sub = f.sv(f.change(sub, later, q.QuoteSHA256, midTerm))
	termEnd := sub.CurrentTerm.EndsAt
	sub = f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, termEnd), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	if sub.CurrentTerm.PriceVersionID != annual.PriceVersionID || !sub.CurrentTerm.EndsAt.Equal(domain.AddBillingIntervals(termEnd, "YEAR", 1, 1)) {
		t.Fatalf("a switch to yearly must start a yearly term at the renewal: %+v", sub.CurrentTerm)
	}
}

func TestChange_QuantityAndAddOnChanges(t *testing.T) {
	f := newSub(t)
	f.publishPlan("teams", planOpts{autoRenew: true, seats: true})
	f.publishPlan("extra_storage", planOpts{kind: domain.ProductKindAddOn, autoRenew: true, base: "10.00"})
	f.rule("teams", "teams", domain.TimingImmediate, domain.ProrationDailyHalfEven)
	sub := f.activeOn("teams", map[string]string{"seats": "5"})

	more := domain.ChangeRequest{Kind: domain.ChangeKindQuantity, PlanQuantities: map[string]string{"seats": "10"}}
	q, err := f.preview(sub, more, midTerm)
	if err != nil {
		t.Fatal(err)
	}
	// 5 included seats; 5 more at the first graduated band (12.00): 49 -> 109.
	if q.OldPeriodCharge != "49.00000000" || q.NewPeriodCharge != "109.00000000" || q.Proration.Net != "38.71" {
		t.Fatalf("seat change quote: old=%s new=%s proration=%+v", q.OldPeriodCharge, q.NewPeriodCharge, q.Proration)
	}
	sub = f.sv(f.change(sub, more, q.QuoteSHA256, midTerm))
	if sub.Effective.Items[0].Quantities[0].Quantity != "10" || sub.Effective.ChangeType != domain.ChangeQuantityChanged {
		t.Fatalf("after the seat change: %+v", sub.Effective.Items[0])
	}
	if _, err := f.preview(sub, more, midTerm.Add(time.Hour)); !errors.Is(err, domain.ErrNoChange) {
		t.Fatalf("an identical configuration was quoted as a change: %v", err)
	}

	add := domain.ChangeRequest{Kind: domain.ChangeKindAddOn, AddOn: &domain.AddOnSelection{ProductCode: "extra_storage",
		Quantities: map[string]string{}, AcceptedTermsSHA256: termsHash}}
	at := midTerm.Add(2 * time.Hour)
	q, err = f.preview(sub, add, at)
	if err != nil {
		t.Fatal(err)
	}
	sub = f.sv(f.change(sub, add, q.QuoteSHA256, at))
	if len(sub.Effective.Items) != 2 || sub.Effective.Items[1].ItemRole != "ADD_ON" {
		t.Fatalf("add-on was not added: %+v", sub.Effective.Items)
	}
	if _, err := f.preview(sub, add, at.Add(time.Hour)); err == nil {
		t.Fatal("the same add-on was quoted twice")
	}
	remove := domain.ChangeRequest{Kind: domain.ChangeKindAddOn, RemoveAddOnCode: "extra_storage"}
	at = at.Add(2 * time.Hour)
	q, err = f.preview(sub, remove, at)
	if err != nil {
		t.Fatal(err)
	}
	sub = f.sv(f.change(sub, remove, q.QuoteSHA256, at))
	if len(sub.Effective.Items) != 1 {
		t.Fatalf("add-on was not removed: %+v", sub.Effective.Items)
	}
	if n := f.outboxCount(sub.SubscriptionID, "subscription.add_on_changed"); n != 2 {
		t.Fatalf("add-on change events: %d", n)
	}
}

func TestChange_OnlyAnActiveSubscriptionChanges(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 0})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationDailyHalfEven)

	pending := f.sv(f.start(f.startParams(f.account, "business", day(11))))
	if _, err := f.preview(pending, planChange("enterprise"), day(12)); !errors.Is(err, domain.ErrSubscriptionInvalidState) {
		t.Fatalf("a pending subscription was quoted a change: %v", err)
	}
	sub := activateNow(f, pending, day(12))
	sub = f.sv(f.s.ScheduleCancellation(f.ctxOrg, f.cmd(sub, "customer-admin", day(13)), f.tclaim(f.org, "customer-admin", "cancel", sub.SubscriptionID)))
	if _, err := f.preview(sub, planChange("enterprise"), day(14)); !errors.Is(err, domain.ErrSubscriptionInvalidState) {
		t.Fatalf("a cancel-pending subscription was quoted a change: %v", err)
	}
}

func TestTransitionRules_SellerOnlyAndRetireOnly(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.publishPlan("extra_storage", planOpts{kind: domain.ProductKindAddOn, autoRenew: true})
	r := f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationDailyHalfEven)

	dup := &domain.PlanTransitionRule{RuleID: domain.NewCommercialID(domain.PrefixTransitionRule), FromProductCode: "business",
		ToProductCode: "enterprise", Timing: domain.TimingNextRenewal, ProrationMethod: domain.ProrationNone, CreatedAt: t0, CreatedByPrincipalID: publisher}
	if _, err := f.s.CreateTransitionRule(f.ctx, dup, f.claim(publisher, "CreateTransitionRule", dup.RuleID)); !errors.Is(err, domain.ErrTransitionRuleExists) {
		t.Fatalf("two active rules for one pair: %v", err)
	}
	toAddOn := &domain.PlanTransitionRule{RuleID: domain.NewCommercialID(domain.PrefixTransitionRule), FromProductCode: "business",
		ToProductCode: "extra_storage", Timing: domain.TimingImmediate, ProrationMethod: domain.ProrationNone, CreatedAt: t0, CreatedByPrincipalID: publisher}
	if _, err := f.s.CreateTransitionRule(f.ctx, toAddOn, f.claim(publisher, "CreateTransitionRule", toAddOn.RuleID)); !errors.Is(err, domain.ErrWrongProductKind) {
		t.Fatalf("a rule into an add-on was accepted: %v", err)
	}
	if _, err := f.tenantExec(orgA, `INSERT INTO plan_transition_rules (rule_id, from_product_id, to_product_id, timing,
		proration_method, created_at, created_by_principal_id) VALUES ('ctr_00000000-0000-4000-8000-000000000001', $1, $2,
		'IMMEDIATE', 'NONE', now(), 'tenant')`, r.FromProductID, r.ToProductID); pgCode(err) != "42501" {
		t.Fatalf("a tenant wrote product policy: %v", err)
	}

	sub := f.activeOn("business", nil)
	if _, err := f.s.RetireTransitionRule(f.ctx, r.RuleID, publisher, "enterprise upgrades now go through sales", day(15)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.preview(sub, planChange("enterprise"), midTerm); !errors.Is(err, domain.ErrTransitionNotAllowed) {
		t.Fatalf("a retired rule still allowed a change: %v", err)
	}
	for _, sql := range []string{
		`UPDATE plan_transition_rules SET timing = 'NEXT_RENEWAL' WHERE rule_id = $1`,
		`DELETE FROM plan_transition_rules WHERE rule_id = $1`,
	} {
		if _, err := f.admin.Exec(f.ctx, sql, r.RuleID); pgCode(err) != "CP001" {
			t.Errorf("%s with RLS bypassed: %v", sql, err)
		}
	}
	if _, err := f.s.RetireTransitionRule(f.ctx, r.RuleID, publisher, "again", day(16)); !errors.Is(err, domain.ErrTransitionRuleRetired) {
		t.Fatalf("a retired rule was retired again: %v", err)
	}
}

func TestChange_IsIdempotent(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationDailyHalfEven)
	sub := f.activeOn("business", nil)
	q, _ := f.preview(sub, planChange("enterprise"), midTerm)

	claim := f.tclaim(f.org, "customer-admin", "change", sub.SubscriptionID)
	c := f.cmd(sub, "customer-admin", midTerm)
	if _, err := f.s.RequestChange(f.ctxOrg, c, planChange("enterprise"), q.QuoteSHA256, claim); err != nil {
		t.Fatal(err)
	}
	c.ChangeID = domain.NewCommercialID(domain.PrefixCommercialChange)
	_, err := f.s.RequestChange(f.ctxOrg, c, planChange("enterprise"), q.QuoteSHA256, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		t.Fatalf("a retried change must replay: %v", err)
	}
	changes, _ := f.s.GetChanges(f.ctxOrg, sub.SubscriptionID)
	if len(changes) != 1 {
		t.Fatalf("%d change rows from one idempotent command", len(changes))
	}
}
