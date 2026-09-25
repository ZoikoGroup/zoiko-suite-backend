package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// COM-02 Subscription against real Postgres (migration 000007), as the
// NOSUPERUSER NOBYPASSRLS role. Times are synthetic and passed explicitly:
// the server clock, never the database's, decides every effective boundary.

const operator = "ops-oliver"

type subFixture struct {
	*pbFixture
	org     string
	account string
	ctxOrg  context.Context
}

func newSub(t *testing.T) *subFixture {
	t.Helper()
	f := &subFixture{pbFixture: newPB(t)}
	f.currency("USD", 2, true)
	f.org, f.account = f.newAccount(orgA, "USD", "GB")
	f.ctxOrg = svcmiddleware.WithTenant(context.Background(), f.org)
	return f
}

// newAccount creates a commercial account for org; an empty market leaves
// it unset.
func (f *subFixture) newAccount(org, currency, market string) (string, string) {
	f.t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), org)
	a := &domain.CommercialAccount{CommercialAccountID: uuid.NewString(), OrganizationID: org, LegalCustomerName: "Acme " + org[:4],
		BillingCurrencyCode: currency, Status: domain.CommercialAccountStatusActive, CreatedAt: t0, CreatedByPrincipalID: "seed"}
	if err := f.s.CreateCommercialAccount(ctx, a); err != nil {
		f.t.Fatalf("create account: %v", err)
	}
	if market != "" {
		if err := f.s.SetAccountMarket(ctx, a.CommercialAccountID, market, operator, t0); err != nil {
			f.t.Fatalf("set market: %v", err)
		}
	}
	return org, a.CommercialAccountID
}

func (f *subFixture) tclaim(org, principal, op, resource string) domain.IdempotencyClaim {
	c := f.claim(principal, op, resource)
	c.OwnerScope = org
	return c
}

type planOpts struct {
	kind      domain.ProductKind
	autoRenew bool
	notice    int
	minTerm   int
	trial     *domain.TrialPolicy
	seats     bool
	base      string // default 49.00
	interval  string // default MONTH
	extra     []domain.PriceComponent
}

var termsHash = strings.Repeat("cd", 32)

// publishPlan publishes a product effective from day 10, published on day 2.
func (f *subFixture) publishPlan(code string, o planOpts) *domain.PriceVersion {
	f.t.Helper()
	if o.kind == "" {
		o.kind = domain.ProductKindPlan
	}
	if o.minTerm == 0 {
		o.minTerm = 1
	}
	p := &domain.Product{ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: code, ProductKind: o.kind, CreatedByPrincipalID: maker}
	if _, err := f.s.CreateProduct(f.ctx, p, f.claim(maker, "CreateProduct", p.ProductID)); err != nil {
		f.t.Fatalf("create product: %v", err)
	}
	if o.base == "" {
		o.base = "49.00"
	}
	if o.interval == "" {
		o.interval = "MONTH"
	}
	d := &domain.PriceVersion{PriceVersionID: domain.NewCommercialID(domain.PrefixPriceVersion), ProductID: p.ProductID,
		DisplayName: code, BillingInterval: o.interval, BillingIntervalCount: 1, CurrencyCode: "USD",
		MarketCodes: []string{"GB", "US"}, EffectiveFrom: day(10), ChangeReason: "test plan", CreatedAt: t0, CreatedByPrincipalID: maker}
	v, err := f.s.CreateDraftVersion(f.ctx, d, false, f.claim(maker, "CreateDraftVersion", d.PriceVersionID))
	if err != nil {
		f.t.Fatalf("create draft: %v", err)
	}
	v = f.put(v, baseComponent(o.base))
	if o.seats {
		v = f.put(v, seatsComponent())
	}
	for _, c := range o.extra {
		v = f.put(v, c)
	}
	v = f.ok(f.s.SetCommercialTerms(f.ctx, v.PriceVersionID, v.RowVersion, &domain.CommercialTerms{
		TermsDocumentRef: "legal/terms/" + code, TermsDocumentSHA256: termsHash, AutoRenew: o.autoRenew,
		RenewalNoticeDays: o.notice, MinimumTermIntervals: o.minTerm, Trial: o.trial, SetAt: t0, SetByPrincipalID: maker,
	}, f.claim(maker, "SetCommercialTerms", v.PriceVersionID)))
	v = f.ok(f.submit(v, maker, t0))
	v = f.ok(f.approve(v, checker, day(1)))
	return f.ok(f.publish(v, publisher, day(2)))
}

func (f *subFixture) startParams(account, code string, at time.Time) domain.StartSubscriptionParams {
	return domain.StartSubscriptionParams{
		SubscriptionID: domain.NewCommercialID(domain.PrefixSubscription), ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange),
		CommercialAccountID: account, PlanProductCode: code, Quantities: map[string]string{}, AcceptedTermsSHA256: termsHash,
		StartsAt: at, Channel: domain.ChannelSelfService, Actor: "customer-admin", Now: at,
	}
}

func (f *subFixture) start(p domain.StartSubscriptionParams) (*domain.SubscriptionView, error) {
	return f.s.StartSubscription(f.ctxOrg, p, f.tclaim(f.org, p.Actor, "StartSubscription", p.SubscriptionID))
}

func (f *subFixture) cmd(v *domain.SubscriptionView, actor string, at time.Time) domain.SubscriptionCommand {
	return domain.SubscriptionCommand{SubscriptionID: v.SubscriptionID, ExpectedVersion: v.RowVersion,
		ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange), Actor: actor, Channel: domain.ChannelSelfService, Now: at}
}

func (f *subFixture) seller(v *domain.SubscriptionView, at time.Time) domain.SubscriptionCommand {
	c := f.cmd(v, operator, at)
	basis := "first invoice paid"
	c.Channel, c.CustomerBasisRef = domain.ChannelAssisted, &basis
	return c
}

func (f *subFixture) sv(v *domain.SubscriptionView, err error) *domain.SubscriptionView {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("unexpected error: %v", err)
	}
	return v
}

func (f *subFixture) statusAt(id string, at time.Time) domain.LifecycleStatus {
	f.t.Helper()
	v, err := f.s.GetSubscriptionAsOf(f.ctxOrg, id, at)
	if err != nil {
		f.t.Fatalf("read subscription at %s: %v", at, err)
	}
	if v.Status == nil {
		return ""
	}
	return *v.Status
}

func activateNow(f *subFixture, v *domain.SubscriptionView, at time.Time) *domain.SubscriptionView {
	f.t.Helper()
	return f.sv(f.s.ActivateSubscription(f.ctxOrg, f.seller(v, at), false, f.tclaim(f.org, operator, "activate", v.SubscriptionID)))
}

// ── Starting ─────────────────────────────────────────────────────────────────

// §4.2 server-resolved context / negative path #05: the customer names a
// product; the server picks the price version, currency and market.
func TestSubscription_StartBindsTheServerResolvedOffer(t *testing.T) {
	f := newSub(t)
	pv := f.publishPlan("business", planOpts{autoRenew: true, seats: true})

	p := f.startParams(f.account, "business", day(11))
	p.Quantities = map[string]string{"seats": "5"}
	v := f.sv(f.start(p))
	if v.Status == nil || *v.Status != domain.LifecyclePending {
		t.Fatalf("a subscription without a trial must start PENDING, got %v", v.Status)
	}
	if v.CurrencyCode != "USD" || v.MarketCode != "GB" {
		t.Fatalf("currency/market must come from the account: %s/%s", v.CurrencyCode, v.MarketCode)
	}
	it := v.Effective.Items[0]
	if it.PriceVersionID != pv.PriceVersionID || it.PriceContentSHA256 != *pv.ContentSHA256 || it.AcceptedTermsSHA256 != termsHash {
		t.Fatalf("binding: %+v", it)
	}
	if len(it.Quantities) != 1 || it.Quantities[0].Quantity != "5" {
		t.Fatalf("quantities: %+v", it.Quantities)
	}
	if n := f.outboxCount(v.SubscriptionID, "subscription.started"); n != 1 {
		t.Fatalf("subscription.started events: %d", n)
	}

	orgC, eurAccount := f.newAccount("33333333-3333-3333-3333-333333333333", "EUR", "GB")
	ctxC := svcmiddleware.WithTenant(context.Background(), orgC)
	eur := f.startParams(eurAccount, "business", day(11))
	eur.Quantities = map[string]string{"seats": "5"}
	if _, err := f.s.StartSubscription(ctxC, eur, f.tclaim(orgC, "customer-admin", "StartSubscription", eur.SubscriptionID)); !errors.Is(err, domain.ErrPriceVersionNotSellable) {
		t.Fatalf("an EUR account was sold a USD price: %v", err)
	}

	orgD, noMarket := f.newAccount("44444444-4444-4444-4444-444444444444", "USD", "")
	ctxD := svcmiddleware.WithTenant(context.Background(), orgD)
	nm := f.startParams(noMarket, "business", day(11))
	if _, err := f.s.StartSubscription(ctxD, nm, f.tclaim(orgD, "customer-admin", "StartSubscription", nm.SubscriptionID)); !errors.Is(err, domain.ErrAccountMarketNotSet) {
		t.Fatalf("sold without a server-resolved market: %v", err)
	}

	stale := f.startParams(f.account, "business", day(11))
	stale.Quantities = map[string]string{"seats": "5"}
	other := domain.NewCommercialID(domain.PrefixPriceVersion)
	stale.ExpectedPriceVersionID = &other
	if _, err := f.start(stale); !errors.Is(err, domain.ErrOfferChanged) {
		t.Fatalf("a client's view of a different price was not refused: %v", err)
	}
	wrongTerms := f.startParams(f.account, "business", day(11))
	wrongTerms.Quantities = map[string]string{"seats": "5"}
	wrongTerms.AcceptedTermsSHA256 = strings.Repeat("0", 64)
	if _, err := f.start(wrongTerms); !errors.Is(err, domain.ErrTermsNotAccepted) {
		t.Fatalf("started without acceptance of the actual terms: %v", err)
	}
}

func TestSubscription_AddOnsBindTheirOwnOffersAndPlanKindIsEnforced(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	addOn := f.publishPlan("extra_storage", planOpts{kind: domain.ProductKindAddOn, autoRenew: true})

	p := f.startParams(f.account, "business", day(11))
	p.AddOns = []domain.AddOnSelection{{ProductCode: "extra_storage", Quantities: map[string]string{}, AcceptedTermsSHA256: termsHash}}
	v := f.sv(f.start(p))
	if len(v.Effective.Items) != 2 || v.Effective.Items[1].ItemRole != "ADD_ON" || v.Effective.Items[1].PriceVersionID != addOn.PriceVersionID {
		t.Fatalf("add-on binding: %+v", v.Effective.Items)
	}

	_, second := f.newAccount(orgB, "USD", "GB")
	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)
	asPlan := f.startParams(second, "extra_storage", day(11))
	if _, err := f.s.StartSubscription(ctxB, asPlan, f.tclaim(orgB, "customer-admin", "StartSubscription", asPlan.SubscriptionID)); !errors.Is(err, domain.ErrWrongProductKind) {
		t.Fatalf("an add-on was sold as a plan: %v", err)
	}
}

// ── Activation and trials ────────────────────────────────────────────────────

// Paid access is never self-granted.
func TestSubscription_PendingIsActivatedBySellerOnly(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	v := f.sv(f.start(f.startParams(f.account, "business", day(11))))

	if _, err := f.s.ActivateSubscription(f.ctxOrg, f.cmd(v, "customer-admin", day(12)), true,
		f.tclaim(f.org, "customer-admin", "activate", v.SubscriptionID)); !errors.Is(err, domain.ErrSellerActivationRequired) {
		t.Fatalf("a customer activated its own paid subscription: %v", err)
	}
	v = activateNow(f, v, day(12))
	if *v.Status != domain.LifecycleActive || v.CurrentTerm == nil || !v.CurrentTerm.StartsAt.Equal(day(12)) ||
		!v.CurrentTerm.EndsAt.Equal(domain.AddBillingIntervals(day(12), "MONTH", 1, 1)) {
		t.Fatalf("activation: status=%v term=%+v", v.Status, v.CurrentTerm)
	}
	if n := f.outboxCount(v.SubscriptionID, "subscription.activated"); n != 1 {
		t.Fatalf("subscription.activated events: %d", n)
	}
	if _, err := f.s.ActivateSubscription(f.ctxOrg, f.seller(v, day(13)), false,
		f.tclaim(f.org, operator, "activate", v.SubscriptionID)); !errors.Is(err, domain.ErrSubscriptionInvalidState) {
		t.Fatalf("an active subscription was activated again: %v", err)
	}
}

// A convert-to-paid trial converts at its end because the version already
// exists, not because a job happened to run.
func TestSubscription_TrialConversionIsScheduledNotPolled(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, trial: &domain.TrialPolicy{DurationDays: 14, Conversion: "CONVERT_TO_PAID"}})
	v := f.sv(f.start(f.startParams(f.account, "business", day(11))))
	trialEnd := day(25)

	if st := f.statusAt(v.SubscriptionID, day(12)); st != domain.LifecycleTrialing {
		t.Fatalf("during the trial: %s", st)
	}
	if st := f.statusAt(v.SubscriptionID, trialEnd); st != domain.LifecycleActive {
		t.Fatalf("at the trial end: %s", st)
	}
	later, _ := f.s.GetSubscriptionAsOf(f.ctxOrg, v.SubscriptionID, day(26))
	if later.CurrentTerm == nil || !later.CurrentTerm.StartsAt.Equal(trialEnd) {
		t.Fatalf("the first paid term must start at the trial end: %+v", later.CurrentTerm)
	}
	ev, err := f.s.GetEffectiveVersion(f.ctxOrg, v.SubscriptionID, day(12))
	if err != nil || ev.LifecycleStatus != domain.LifecycleTrialing || ev.EffectiveTo == nil || !ev.EffectiveTo.Equal(trialEnd) {
		t.Fatalf("the trial version must end where the paid one begins: %+v, %v", ev, err)
	}
	if _, err := f.s.GetEffectiveVersion(f.ctxOrg, v.SubscriptionID, day(10)); !errors.Is(err, store.ErrNoEffectiveVersion) {
		t.Fatalf("an as-of read before the start must say so: %v", err)
	}
}

// Negative path #07 in spirit: what happens at a trial's end is what was
// disclosed — here, expiry unless the customer confirms.
func TestSubscription_TrialNeedingConfirmationExpiresUnlessConfirmed(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, trial: &domain.TrialPolicy{DurationDays: 14, Conversion: "REQUIRE_CONFIRMATION"}})
	v := f.sv(f.start(f.startParams(f.account, "business", day(11))))
	if st := f.statusAt(v.SubscriptionID, day(26)); st != domain.LifecycleExpired || v.EndsAt == nil || !v.EndsAt.Equal(day(25)) {
		t.Fatalf("an unconfirmed trial must expire at its end: %s, ends_at=%v", st, v.EndsAt)
	}

	v = f.sv(f.s.ActivateSubscription(f.ctxOrg, f.cmd(v, "customer-admin", day(12)), true,
		f.tclaim(f.org, "customer-admin", "activate", v.SubscriptionID)))
	if st := f.statusAt(v.SubscriptionID, day(26)); st != domain.LifecycleActive || v.EndsAt != nil {
		t.Fatalf("a confirmed trial must convert at its end: %s, ends_at=%v", st, v.EndsAt)
	}
	history, _ := f.s.GetChangeHistory(f.ctxOrg, v.SubscriptionID)
	var voided int
	for _, h := range history {
		if h.VoidedAt != nil {
			voided++
			if h.LifecycleStatus != domain.LifecycleExpired || h.VoidedByChangeID == nil {
				t.Fatalf("the voided version must be the withdrawn expiry, with its cause: %+v", h)
			}
		}
	}
	if voided != 1 {
		t.Fatalf("history must keep the withdrawn expiry as a voided version, found %d", voided)
	}
	if _, err := f.s.ActivateSubscription(f.ctxOrg, f.cmd(v, "customer-admin", day(13)), true,
		f.tclaim(f.org, "customer-admin", "activate", v.SubscriptionID)); !errors.Is(err, domain.ErrConversionAlreadyScheduled) {
		t.Fatalf("a second confirmation: %v", err)
	}
}

// ── Price binding ────────────────────────────────────────────────────────────

// COM-CTRL-003 / negative path #04: a newer published price reprices nobody,
// not even at renewal.
func TestSubscription_GrandfatheredAcrossANewPriceAndARenewal(t *testing.T) {
	f := newSub(t)
	v1 := f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	v2 := f.draft(v1.ProductID, day(40), true, maker)
	v2 = f.put(v2, baseComponent("59.00"))
	v2 = f.ok(f.submit(v2, maker, day(20)))
	v2 = f.ok(f.approve(v2, checker, day(21)))
	f.ok(f.publish(v2, publisher, day(22)))

	termEnd := sub.CurrentTerm.EndsAt
	renewAt := termEnd.Add(time.Hour)
	renewed := f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, renewAt), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	if renewed.CurrentTerm == nil || renewed.CurrentTerm.TermNo != 2 || renewed.CurrentTerm.PriceVersionID != v1.PriceVersionID {
		t.Fatalf("renewal must keep the bound price version: %+v", renewed.CurrentTerm)
	}
	if it := renewed.Effective.Items[0]; it.PriceVersionID != v1.PriceVersionID || it.PriceContentSHA256 != *v1.ContentSHA256 {
		t.Fatalf("the subscription was repriced: %+v", it)
	}
	if n := f.outboxCount(sub.SubscriptionID, "subscription.renewed"); n != 1 {
		t.Fatalf("subscription.renewed events: %d", n)
	}
}

// ── Cancellation ─────────────────────────────────────────────────────────────

func TestSubscription_ScheduledCancellationHonoursNoticeAndCanBeWithdrawn(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 30})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	anchor := sub.CurrentTerm.StartsAt

	requestedAt := day(19)
	sub = f.sv(f.s.ScheduleCancellation(f.ctxOrg, f.cmd(sub, "customer-admin", requestedAt), f.tclaim(f.org, "customer-admin", "cancel", sub.SubscriptionID)))
	boundary := domain.AddBillingIntervals(anchor, "MONTH", 1, 2) // term 1 ends inside the 30-day notice window
	if sub.EndsAt == nil || !sub.EndsAt.Equal(boundary) {
		t.Fatalf("cancellation boundary = %v, want %s", sub.EndsAt, boundary)
	}
	if st := f.statusAt(sub.SubscriptionID, day(20)); st != domain.LifecycleCancelPending {
		t.Fatalf("before the boundary the subscription stays in service as CANCEL_PENDING, got %s", st)
	}
	if st := f.statusAt(sub.SubscriptionID, boundary); st != domain.LifecycleCanceled {
		t.Fatalf("at the boundary: %s", st)
	}
	if _, err := f.s.ScheduleCancellation(f.ctxOrg, f.cmd(sub, "customer-admin", day(21)),
		f.tclaim(f.org, "customer-admin", "cancel", sub.SubscriptionID)); !errors.Is(err, domain.ErrSubscriptionInvalidState) {
		t.Fatalf("a cancellation was scheduled twice: %v", err)
	}

	sub = f.sv(f.s.Reactivate(f.ctxOrg, f.cmd(sub, "customer-admin", day(25)), f.tclaim(f.org, "customer-admin", "reactivate", sub.SubscriptionID)))
	if st := f.statusAt(sub.SubscriptionID, boundary.Add(time.Hour)); st != domain.LifecycleActive || sub.EndsAt != nil {
		t.Fatalf("after reactivation: %s, ends_at=%v", st, sub.EndsAt)
	}
	// The past is unchanged: at day 20 it WAS cancel-pending.
	if st := f.statusAt(sub.SubscriptionID, day(20)); st != domain.LifecycleCancelPending {
		t.Fatalf("reactivation rewrote history: at day 20 the subscription now reads %s", st)
	}
}

func TestSubscription_CancelNowRespectsTheMinimumTerm(t *testing.T) {
	f := newSub(t)
	f.publishPlan("annual_commit", planOpts{autoRenew: true, minTerm: 12})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "annual_commit", day(11)))), day(11))

	if _, err := f.s.CancelNow(f.ctxOrg, f.cmd(sub, "customer-admin", day(90)),
		f.tclaim(f.org, "customer-admin", "cancel-now", sub.SubscriptionID)); !errors.Is(err, domain.ErrMinimumTermNotMet) {
		t.Fatalf("cancelled inside a 12-month minimum term: %v", err)
	}
	served := domain.AddBillingIntervals(day(11), "MONTH", 1, 12)
	sub = f.sv(f.s.CancelNow(f.ctxOrg, f.cmd(sub, "customer-admin", served), f.tclaim(f.org, "customer-admin", "cancel-now", sub.SubscriptionID)))
	if *sub.Status != domain.LifecycleCanceled || sub.EndsAt == nil || !sub.EndsAt.Equal(served) {
		t.Fatalf("cancel after the minimum term: %v ends_at=%v", sub.Status, sub.EndsAt)
	}
	if _, err := f.s.CancelNow(f.ctxOrg, f.cmd(sub, "customer-admin", served.Add(time.Hour)),
		f.tclaim(f.org, "customer-admin", "cancel-now", sub.SubscriptionID)); !errors.Is(err, domain.ErrSubscriptionEnded) {
		t.Fatalf("a canceled subscription was canceled again: %v", err)
	}
}

// ── Overlap, legacy exclusion, idempotency ───────────────────────────────────

func TestSubscription_OneLiveSubscriptionPerAccount(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	first := f.sv(f.start(f.startParams(f.account, "business", day(11))))
	if _, err := f.start(f.startParams(f.account, "business", day(12))); !errors.Is(err, domain.ErrSubscriptionOverlap) {
		t.Fatalf("a second overlapping subscription was sold to one account: %v", err)
	}

	first = f.sv(f.s.CancelNow(f.ctxOrg, f.cmd(first, "customer-admin", day(13)), f.tclaim(f.org, "customer-admin", "cancel-now", first.SubscriptionID)))
	if _, err := f.start(f.startParams(f.account, "business", day(13))); err != nil {
		t.Fatalf("a new subscription after the first ended must be allowed: %v", err)
	}

	// Concurrent starts on a fresh account: exactly one wins.
	org, acct := f.newAccount(orgB, "USD", "GB")
	ctx := svcmiddleware.WithTenant(context.Background(), org)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := f.startParams(acct, "business", day(11))
			_, errs[i] = f.s.StartSubscription(ctx, p, f.tclaim(org, fmt.Sprintf("user-%d", i), "StartSubscription", p.SubscriptionID))
		}(i)
	}
	wg.Wait()
	var won int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, domain.ErrSubscriptionOverlap):
			t.Fatalf("unexpected concurrent-start error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent subscriptions were created for one account", won)
	}
}

// An account can hold a live subscription in the doc7 model or in COM-02,
// never both: that would bill the customer twice.
func TestSubscription_MutuallyExclusiveWithDoc7Subscriptions(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})

	catalog := &domain.PriceCatalog{CatalogVersionID: uuid.NewString(), CatalogCode: "legacy-2026", Status: domain.CatalogStatusPublished,
		EffectiveFrom: t0, CreatedAt: t0, CreatedByPrincipalID: "seed"}
	if err := f.s.CreatePriceCatalog(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	plan := &domain.Plan{PlanID: uuid.NewString(), CatalogVersionID: catalog.CatalogVersionID, PlanCode: "GROWTH", DisplayName: "Growth",
		BillingInterval: "MONTHLY", BasePriceAmount: 499, BasePriceCurrencyCode: "USD", CreatedAt: t0, CreatedByPrincipalID: "seed"}
	if err := f.s.CreatePlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	legacy := func(ctx context.Context, account string) error {
		return f.s.CreateSubscription(ctx, &domain.CommercialSubscription{SubscriptionID: uuid.NewString(), CommercialAccountID: account,
			PlanID: plan.PlanID, CatalogVersionID: catalog.CatalogVersionID, BillingInterval: "MONTHLY",
			Status: domain.SubscriptionStatusActive, CreatedAt: t0, UpdatedAt: t0, CreatedByPrincipalID: "seed"})
	}

	if err := legacy(f.ctxOrg, f.account); err != nil {
		t.Fatalf("seed doc7 subscription: %v", err)
	}
	if _, err := f.start(f.startParams(f.account, "business", day(11))); !errors.Is(err, domain.ErrLegacySubscriptionLive) {
		t.Fatalf("a COM-02 subscription was sold on top of a live doc7 one: %v", err)
	}

	org, acct := f.newAccount(orgB, "USD", "GB")
	ctx := svcmiddleware.WithTenant(context.Background(), org)
	p := f.startParams(acct, "business", day(11))
	if _, err := f.s.StartSubscription(ctx, p, f.tclaim(org, "customer-admin", "StartSubscription", p.SubscriptionID)); err != nil {
		t.Fatal(err)
	}
	if err := legacy(ctx, acct); !errors.Is(err, domain.ErrActiveSubscriptionExists) {
		t.Fatalf("a doc7 subscription was created on top of a live COM-02 one: %v", err)
	}
}

func TestSubscription_StartIsIdempotent(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	p := f.startParams(f.account, "business", day(11))
	claim := f.tclaim(f.org, p.Actor, "StartSubscription", p.SubscriptionID)
	first := f.sv(f.s.StartSubscription(f.ctxOrg, p, claim))

	retry := p
	retry.SubscriptionID = domain.NewCommercialID(domain.PrefixSubscription)
	_, err := f.s.StartSubscription(f.ctxOrg, retry, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.SubscriptionID {
		t.Fatalf("a retried start must replay the first subscription: %v", err)
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM subscriptions WHERE commercial_account_id = $1`, f.account).Scan(&n)
	if n != 1 {
		t.Fatalf("%d subscriptions created by one idempotent command", n)
	}
}

func TestSubscription_StaleETagIsRefused(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	v := f.sv(f.start(f.startParams(f.account, "business", day(11))))
	c := f.seller(v, day(12))
	c.ExpectedVersion = v.RowVersion + 1
	if _, err := f.s.ActivateSubscription(f.ctxOrg, c, false, f.tclaim(f.org, operator, "activate", v.SubscriptionID)); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("a command against a stale ETag was applied: %v", err)
	}
}

// ── Renewal ──────────────────────────────────────────────────────────────────

func TestSubscription_RenewalRules(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	if _, err := f.s.Renew(f.ctxOrg, f.seller(sub, day(20)), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)); !errors.Is(err, domain.ErrRenewalNotDue) {
		t.Fatalf("an auto-renewing subscription renewed mid-term: %v", err)
	}
	rs, _ := f.s.GetRenewalState(f.ctxOrg, sub.SubscriptionID, sub.CurrentTerm.EndsAt)
	if !rs.AutoRenew || !rs.RenewalDue || rs.NextTermStartsAt == nil || !rs.NextTermStartsAt.Equal(sub.CurrentTerm.EndsAt) {
		t.Fatalf("renewal state at term end: %+v", rs)
	}

	// A term that does not auto-renew expires unless renewed before its end.
	org, acct := f.newAccount(orgB, "USD", "GB")
	ctx := svcmiddleware.WithTenant(context.Background(), org)
	f.publishPlan("fixed_term", planOpts{autoRenew: false})
	p := f.startParams(acct, "fixed_term", day(11))
	fixed := f.sv(f.s.StartSubscription(ctx, p, f.tclaim(org, "customer-admin", "StartSubscription", p.SubscriptionID)))
	fixed = f.sv(f.s.ActivateSubscription(ctx, f.seller(fixed, day(11)), false, f.tclaim(org, operator, "activate", fixed.SubscriptionID)))
	end := fixed.CurrentTerm.EndsAt
	if fixed.EndsAt == nil || !fixed.EndsAt.Equal(end) {
		t.Fatalf("a non-renewing term must end the subscription at its end: %v", fixed.EndsAt)
	}
	renewed := f.sv(f.s.Renew(ctx, f.cmd(fixed, "customer-admin", end.Add(-24*time.Hour)), f.tclaim(org, "customer-admin", "renew", fixed.SubscriptionID)))
	newEnd := domain.AddBillingIntervals(day(11), "MONTH", 1, 2)
	if renewed.EndsAt == nil || !renewed.EndsAt.Equal(newEnd) {
		t.Fatalf("manual renewal must move the expiry to the new term's end: %v, want %s", renewed.EndsAt, newEnd)
	}
	ctxOrg := f.ctxOrg
	f.ctxOrg = ctx
	if st := f.statusAt(fixed.SubscriptionID, end.Add(time.Hour)); st != domain.LifecycleActive {
		t.Fatalf("after renewal the old expiry must not apply: %s", st)
	}
	f.ctxOrg = ctxOrg
	if _, err := f.s.Renew(ctx, f.cmd(renewed, "customer-admin", newEnd.Add(time.Hour)), f.tclaim(org, "customer-admin", "renew", fixed.SubscriptionID)); !errors.Is(err, domain.ErrSubscriptionEnded) {
		t.Fatalf("an expired subscription was renewed: %v", err)
	}
}

// ── Append-only history and isolation ────────────────────────────────────────

func TestSubscription_HistoryIsAppendOnly(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	effective := sub.Effective.SubscriptionVersionID

	raw := map[string]string{
		"rewrite status":  `UPDATE subscription_versions SET lifecycle_status = 'CANCELED' WHERE subscription_version_id = $1`,
		"delete version":  `DELETE FROM subscription_versions WHERE subscription_version_id = $1`,
		"repoint item":    `UPDATE subscription_items SET price_content_sha256 = repeat('0', 64) WHERE subscription_version_id = $1`,
		"change quantity": `DELETE FROM subscription_items WHERE subscription_version_id = $1`,
		"rebind product":  `UPDATE subscriptions SET product_id = product_id, starts_at = starts_at - interval '1 day', row_version = row_version + 1 WHERE subscription_id = (SELECT subscription_id FROM subscription_versions WHERE subscription_version_id = $1)`,
		"rewrite term":    `UPDATE subscription_terms SET ends_at = ends_at + interval '1 year' WHERE subscription_id = (SELECT subscription_id FROM subscription_versions WHERE subscription_version_id = $1)`,
	}
	for name, sql := range raw {
		if _, err := f.tenantExec(f.org, sql, effective); pgCode(err) != "CP001" {
			t.Errorf("%s (application role): got %v, want CP001", name, err)
		}
		if _, err := f.admin.Exec(f.ctx, sql, effective); pgCode(err) != "CP001" {
			t.Errorf("%s (RLS bypassed): got %v, want CP001", name, err)
		}
	}
	// Voiding is the one permitted change, and only before a version takes
	// effect: voiding the version already in force is refused.
	if _, err := f.tenantExec(f.org, `UPDATE subscription_versions SET voided_at = $2, voided_by_principal_id = 'x',
		void_reason = 'x', voided_by_change_id = 'x' WHERE subscription_version_id = $1`, effective, day(30)); pgCode(err) != "23514" {
		t.Fatalf("a version already in force was voided: %v", err)
	}
}

func TestSubscription_TenantIsolation(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := f.sv(f.start(f.startParams(f.account, "business", day(11))))

	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)
	if _, err := f.s.GetSubscriptionAsOf(ctxB, sub.SubscriptionID, day(12)); !errors.Is(err, domain.ErrSubscriptionNotFound) {
		t.Fatalf("ISOLATION FAILURE: org B read org A's subscription: %v", err)
	}
	if _, err := f.s.GetChangeHistory(ctxB, sub.SubscriptionID); !errors.Is(err, domain.ErrSubscriptionNotFound) {
		t.Fatalf("ISOLATION FAILURE: org B read org A's history: %v", err)
	}

	insert := `INSERT INTO subscriptions (subscription_id, organization_id, commercial_account_id, product_id, currency_code,
		market_code, channel, starts_at, created_at, created_by_principal_id)
		SELECT 'csub_` + uuid.NewString() + `', $1::uuid, $2::uuid, product_id, 'USD', 'GB', 'SELF_SERVICE', $3, $3, 'mallory'
		FROM commercial_products LIMIT 1`
	// Postgres runs BEFORE triggers ahead of the RLS WITH CHECK, so which
	// layer refuses depends on ordering: the account trigger (it cannot see
	// org A's account from org B) or the policy. Either is a refusal.
	if _, err := f.tenantExec(orgB, insert, orgA, f.account, day(50)); pgCode(err) != "42501" && pgCode(err) != "CP002" {
		t.Fatalf("org B wrote a subscription into org A: %v", err)
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM subscriptions WHERE created_by_principal_id = 'mallory'`).Scan(&n)
	if n != 0 {
		t.Fatalf("ISOLATION FAILURE: %d cross-tenant subscription rows exist", n)
	}
	if _, err := f.tenantExec(orgB, insert, orgB, f.account, day(50)); pgCode(err) != "CP002" {
		t.Fatalf("org B attached a subscription to org A's account: %v", err)
	}
}
