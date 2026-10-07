package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// COM-02 part 2c against real Postgres as the NOBYPASSRLS role: the
// boundary queue and worker, discounts under delegated authority (#06), and
// price migration offers (#38).

func (f *subFixture) queueCount(subID, status string) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM subscription_boundary_queue WHERE subscription_id = $1 AND status = $2`,
		subID, status).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *subFixture) drain(at time.Time) int {
	f.t.Helper()
	n := 0
	for {
		found, err := f.s.ProcessNextBoundary(f.ctx, at)
		if err != nil {
			f.t.Fatalf("boundary processing: %v", err)
		}
		if !found {
			return n
		}
		n++
	}
}

func (f *subFixture) liveTermCount(subID string) int {
	f.t.Helper()
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM subscription_terms WHERE subscription_id = $1 AND voided_at IS NULL`, subID).Scan(&n)
	return n
}

// ── Boundary worker ──────────────────────────────────────────────────────────

// A convert-to-paid trial: the conversion event fires when the trial ends,
// and the first term renews itself when it ends — each exactly once.
func TestBoundary_PublishesAtEffectiveTimeAndRenewsAtTermEnd(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, trial: &domain.TrialPolicy{DurationDays: 14, Conversion: "CONVERT_TO_PAID"}})
	sub := f.sv(f.start(f.startParams(f.account, "business", day(11))))
	trialEnd := day(25)

	if n := f.drain(trialEnd.Add(-time.Minute)); n != 0 {
		t.Fatalf("%d boundaries processed before anything was due", n)
	}
	if n := f.drain(trialEnd); n != 1 || f.outboxCount(sub.SubscriptionID, "subscription.activated") != 1 {
		t.Fatalf("at the trial end: processed %d, activation events %d", n, f.outboxCount(sub.SubscriptionID, "subscription.activated"))
	}
	termEnd := domain.AddBillingIntervals(trialEnd, "MONTH", 1, 1)
	if n := f.drain(termEnd.Add(-time.Minute)); n != 0 {
		t.Fatalf("a term renewed before its end: %d", n)
	}
	if n := f.drain(termEnd); n != 1 || f.liveTermCount(sub.SubscriptionID) != 2 {
		t.Fatalf("at the term end: processed %d, terms %d", n, f.liveTermCount(sub.SubscriptionID))
	}
	if f.outboxCount(sub.SubscriptionID, "subscription.renewed") != 1 {
		t.Fatal("the renewal event was not published")
	}
	// The renewal queued the next term's end: the chain continues on its own.
	if f.queueCount(sub.SubscriptionID, "PENDING") != 1 {
		t.Fatalf("the renewed term's own end was not queued")
	}
	if n := f.drain(termEnd.Add(time.Hour)); n != 0 {
		t.Fatalf("a processed boundary ran again: %d", n)
	}
}

// Nothing is trusted from scheduling time: a withdrawn cancellation is
// skipped, and a term that ends with the subscription is not renewed.
func TestBoundary_SkipsWhatNoLongerApplies(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 0})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	termEnd := sub.CurrentTerm.EndsAt

	sub = f.sv(f.s.ScheduleCancellation(f.ctxOrg, f.cmd(sub, "customer-admin", day(20)), f.tclaim(f.org, "customer-admin", "cancel", sub.SubscriptionID)))
	sub = f.sv(f.s.Reactivate(f.ctxOrg, f.cmd(sub, "customer-admin", day(21)), f.tclaim(f.org, "customer-admin", "reactivate", sub.SubscriptionID)))
	sub = f.sv(f.s.ScheduleCancellation(f.ctxOrg, f.cmd(sub, "customer-admin", day(22)), f.tclaim(f.org, "customer-admin", "cancel", sub.SubscriptionID)))

	f.drain(termEnd)
	if f.outboxCount(sub.SubscriptionID, "subscription.canceled") != 1 {
		t.Fatalf("the cancellation that stands must fire exactly once, got %d", f.outboxCount(sub.SubscriptionID, "subscription.canceled"))
	}
	var voided, ended int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE outcome = 'SKIPPED_VOIDED'), count(*) FILTER (WHERE outcome = 'SKIPPED_ENDED')
		FROM subscription_boundary_queue WHERE subscription_id = $1`, sub.SubscriptionID).Scan(&voided, &ended)
	if voided != 1 || ended != 1 || f.liveTermCount(sub.SubscriptionID) != 1 {
		t.Fatalf("skips: voided=%d ended=%d terms=%d (a canceled subscription must not renew)", voided, ended, f.liveTermCount(sub.SubscriptionID))
	}
}

// Workers in every replica: one due term end, many workers, one renewal.
func TestBoundary_ConcurrentWorkersRenewOnce(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	termEnd := sub.CurrentTerm.EndsAt

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				found, err := f.s.ProcessNextBoundary(context.Background(), termEnd)
				if err != nil || !found {
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := f.liveTermCount(sub.SubscriptionID); n != 2 {
		t.Fatalf("%d terms after concurrent workers; exactly one renewal must happen", n)
	}
}

// A failing item backs off and is parked after repeated failures, and never
// blocks the healthy item behind it.
func TestBoundary_FailingItemBacksOffAndIsParked(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO subscription_boundary_queue
		(organization_id, subscription_id, kind, term_no, due_at, next_attempt_at)
		VALUES ($1, $2, 'TERM_END', 99, $3, $3)`, f.org, sub.SubscriptionID, day(12)); err != nil {
		t.Fatal(err)
	}

	at := day(12)
	found, err := f.s.ProcessNextBoundary(f.ctx, at)
	if !found || err == nil {
		t.Fatalf("a broken item must be reported as a failure: found=%v err=%v", found, err)
	}
	var attempts int
	var next time.Time
	_ = f.admin.QueryRow(f.ctx, `SELECT attempts, next_attempt_at FROM subscription_boundary_queue WHERE term_no = 99`).Scan(&attempts, &next)
	if attempts != 1 || !next.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("backoff: attempts=%d next=%s", attempts, next)
	}
	if found, _ := f.s.ProcessNextBoundary(f.ctx, at); found {
		t.Fatal("the failed item was retried before its backoff elapsed")
	}
	for i := 0; i < 20; i++ {
		at = at.Add(2 * time.Hour)
		_, _ = f.s.ProcessNextBoundary(f.ctx, at)
	}
	var status string
	_ = f.admin.QueryRow(f.ctx, `SELECT status, attempts FROM subscription_boundary_queue WHERE term_no = 99`).Scan(&status, &attempts)
	if status != "FAILED" || attempts != 10 {
		t.Fatalf("after repeated failures: status=%s attempts=%d, want FAILED after 10", status, attempts)
	}
	f.drain(sub.CurrentTerm.EndsAt)
	if n := f.liveTermCount(sub.SubscriptionID); n != 2 {
		t.Fatalf("the healthy renewal behind the failed item did not run: %d terms", n)
	}
}

func TestBoundary_QueueIsTenantIsolated(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	var own, foreign int
	_ = f.app.QueryRow(f.ctx, `SELECT count(*) FROM subscription_boundary_queue`).Scan(&foreign)
	if foreign != 0 {
		t.Fatalf("the queue was readable with no tenant or worker declared: %d rows", foreign)
	}
	tx, _ := f.app.Begin(f.ctx)
	defer tx.Rollback(f.ctx) //nolint:errcheck
	_, _ = tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgB)
	_ = tx.QueryRow(f.ctx, `SELECT count(*) FROM subscription_boundary_queue`).Scan(&own)
	if own != 0 {
		t.Fatalf("org B read org A's boundaries: %d", own)
	}
}

// ── Discounts ────────────────────────────────────────────────────────────────

func discountComponent(key string, requiresApproval bool) domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: key, ComponentType: domain.ComponentDiscount, DiscountType: strp("PERCENT"),
		DiscountValue: strp("15"), DurationIntervals: intp(12), EligibilityCode: strp("ANNUAL_COMMIT"), RequiresApproval: boolp(requiresApproval)}
}

func (f *subFixture) propose(sub *domain.SubscriptionView, product, key, by string) (*domain.DiscountApplication, error) {
	id := domain.NewCommercialID(domain.PrefixDiscountApplication)
	return f.s.ProposeDiscount(f.ctxOrg, domain.ProposeDiscountParams{DiscountApplicationID: id, SubscriptionID: sub.SubscriptionID,
		ProductCode: product, ComponentKey: key, Reason: "multi-year deal", CustomerBasisRef: "order form SO-77", Actor: by, Now: day(20)},
		f.tclaim(f.org, by, "ProposeDiscount", id))
}

func (f *subFixture) decide(sub *domain.SubscriptionView, d *domain.DiscountApplication, decision, by, reason string) (*domain.DiscountApplication, error) {
	return f.s.DecideDiscount(f.ctxOrg, sub.SubscriptionID, d.DiscountApplicationID, d.RowVersion, decision, by, reason, day(21),
		f.tclaim(f.org, by, "Discount:"+decision, d.DiscountApplicationID))
}

// Negative path #06 / COM-CTRL-005: a sales user cannot approve their own
// material discount — not through the store, not through raw SQL.
func TestDiscount_ProposerCannotApprove(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, extra: []domain.PriceComponent{
		discountComponent("commit_15", true), discountComponent("standard_promo", false)}})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	d, err := f.propose(sub, "business", "commit_15", "sales-sam")
	if err != nil || d.Status != domain.DiscountProposed {
		t.Fatalf("propose: %+v %v", d, err)
	}
	if _, err := f.decide(sub, d, "approve", "sales-sam", ""); !errors.Is(err, domain.ErrSoDViolation) {
		t.Fatalf("a proposer approved their own discount: %v", err)
	}
	if _, err := f.tenantExec(f.org, `UPDATE subscription_discounts SET status = 'APPROVED', row_version = row_version + 1,
		approval_basis = 'APPROVER', approved_by_principal_id = requested_by_principal_id, approved_at = now()
		WHERE discount_application_id = $1`, d.DiscountApplicationID); pgCode(err) != "23514" {
		t.Fatalf("a raw self-approval was not refused by the schema: %v", err)
	}
	if _, err := f.propose(sub, "business", "commit_15", "sales-sid"); !errors.Is(err, domain.ErrDiscountExists) {
		t.Fatalf("a second live application of one discount: %v", err)
	}
	approved, err := f.decide(sub, d, "approve", "finance-fiona", "")
	if err != nil || approved.Status != domain.DiscountApproved || *approved.ApprovalBasis != domain.ApprovalByApprover {
		t.Fatalf("an independent approver: %+v %v", approved, err)
	}

	auto, err := f.propose(sub, "business", "standard_promo", "sales-sam")
	if err != nil || auto.Status != domain.DiscountApproved || *auto.ApprovalBasis != domain.ApprovalCatalogPolicy {
		t.Fatalf("a catalogue-approved discount must be approved on catalogue policy, and say so: %+v %v", auto, err)
	}
	if _, err := f.propose(sub, "business", "no_such_discount", "sales-sam"); !errors.Is(err, domain.ErrDiscountNotApplicable) {
		t.Fatalf("a discount the catalogue does not define was applied: %v", err)
	}
	if n := f.outboxCount(d.DiscountApplicationID, "subscription.discount_approved"); n != 1 {
		t.Fatalf("approval events: %d", n)
	}
}

func TestDiscount_DecisionsNeedTheirOwnAuthority(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, extra: []domain.PriceComponent{discountComponent("commit_15", true)}})
	f.publishPlan("enterprise", planOpts{autoRenew: true, base: "99.00"})
	f.rule("business", "enterprise", domain.TimingImmediate, domain.ProrationNone)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	d, _ := f.propose(sub, "business", "commit_15", "sales-sam")
	if _, err := f.decide(sub, d, "withdraw", "sales-sid", "wrong account"); !errors.Is(err, domain.ErrDiscountNotRequester) {
		t.Fatalf("someone else withdrew a proposal: %v", err)
	}
	withdrawn, err := f.decide(sub, d, "withdraw", "sales-sam", "customer declined")
	if err != nil || withdrawn.Status != domain.DiscountWithdrawn {
		t.Fatalf("withdraw: %+v %v", withdrawn, err)
	}
	if _, err := f.decide(sub, withdrawn, "approve", "finance-fiona", ""); !errors.Is(err, domain.ErrDiscountInvalidState) {
		t.Fatalf("a withdrawn discount was approved: %v", err)
	}

	// The discounted price version changes before approval: nothing is left
	// to approve (a protected-term change invalidates the approval subject).
	d, _ = f.propose(sub, "business", "commit_15", "sales-sam")
	q, err := f.preview(sub, planChange("enterprise"), day(20))
	if err != nil {
		t.Fatal(err)
	}
	f.sv(f.change(sub, planChange("enterprise"), q.QuoteSHA256, day(20)))
	if _, err := f.decide(sub, d, "approve", "finance-fiona", ""); !errors.Is(err, domain.ErrDiscountNotApplicable) {
		t.Fatalf("a discount on a price version no longer bound was approved: %v", err)
	}
}

// ── Migration offers ─────────────────────────────────────────────────────────

func (f *subFixture) offer(o *domain.MigrationOffer, by string) (*domain.MigrationOffer, error) {
	o.MigrationOfferID = domain.NewCommercialID(domain.PrefixMigrationOffer)
	o.CreatedAt, o.CreatedByPrincipalID = t0, by
	if o.Reason == "" {
		o.Reason = "2030 repricing"
	}
	return f.s.CreateMigrationOffer(f.ctx, o, f.claim(by, "CreateMigrationOffer", o.MigrationOfferID))
}

func (f *subFixture) publishOffer(o *domain.MigrationOffer, by string, at time.Time) (*domain.MigrationOffer, error) {
	return f.s.PublishMigrationOffer(f.ctx, o.MigrationOfferID, o.RowVersion, by, at, f.claim(by, "publish-offer", o.MigrationOfferID))
}

func TestMigration_OfferNeedsEligibilityAndASecondPerson(t *testing.T) {
	f := newSub(t)
	v1 := f.publishPlan("business", planOpts{autoRenew: true})
	v2 := f.draft(v1.ProductID, day(40), true, maker)
	v2 = f.put(v2, baseComponent("59.00"))
	v2 = f.ok(f.submit(v2, maker, day(20)))
	v2 = f.ok(f.approve(v2, checker, day(21)))
	v2 = f.ok(f.publish(v2, publisher, day(22)))
	subA := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	orgB2, acctB := f.newAccount(orgB, "USD", "GB")
	ctxB := svcmiddleware.WithTenant(context.Background(), orgB2)
	pB := f.startParams(acctB, "business", day(11))
	subB := f.sv(f.s.StartSubscription(ctxB, pB, f.tclaim(orgB2, "customer-admin", "StartSubscription", pB.SubscriptionID)))
	subB = f.sv(f.s.ActivateSubscription(ctxB, f.seller(subB, day(11)), false, f.tclaim(orgB2, operator, "activate", subB.SubscriptionID)))

	// Negative path #38: no eligibility rule, no publication.
	noRule, err := f.offer(&domain.MigrationOffer{FromPriceVersionID: v1.PriceVersionID, ToPriceVersionID: v2.PriceVersionID}, maker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publishOffer(noRule, checker, day(23)); !errors.Is(err, domain.ErrMigrationEligibilityMissing) {
		t.Fatalf("an offer with no eligibility rule was published: %v", err)
	}
	listed := domain.EligibilityListedSubscriptions
	emptyList, _ := f.offer(&domain.MigrationOffer{FromPriceVersionID: v1.PriceVersionID, ToPriceVersionID: v2.PriceVersionID, EligibilityMode: &listed}, maker)
	if _, err := f.publishOffer(emptyList, checker, day(23)); !errors.Is(err, domain.ErrMigrationEligibilityMissing) {
		t.Fatalf("a subset offer listing nobody was published: %v", err)
	}
	if _, err := f.offer(&domain.MigrationOffer{FromPriceVersionID: v2.PriceVersionID, ToPriceVersionID: v1.PriceVersionID, EligibilityMode: &listed}, maker); !errors.Is(err, domain.ErrMigrationTargetInvalid) {
		t.Fatalf("a migration backwards to an older price was accepted: %v", err)
	}

	o, err := f.offer(&domain.MigrationOffer{FromPriceVersionID: v1.PriceVersionID, ToPriceVersionID: v2.PriceVersionID,
		EligibilityMode: &listed, TargetSubscriptionIDs: []string{subA.SubscriptionID}}, maker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publishOffer(o, maker, day(23)); !errors.Is(err, domain.ErrSoDViolation) {
		t.Fatalf("the creator published their own migration offer: %v", err)
	}
	o, err = f.publishOffer(o, checker, day(23))
	if err != nil || o.Status != domain.MigrationOfferPublished {
		t.Fatalf("publish: %+v %v", o, err)
	}
	if _, err := f.sellerExec(`INSERT INTO price_migration_offer_targets (migration_offer_id, subscription_id) VALUES ($1, $2)`,
		o.MigrationOfferID, subB.SubscriptionID); pgCode(err) != "CP001" {
		t.Fatalf("a target was added to a published offer: %v", err)
	}

	offersA, _ := f.s.ListMigrationOffersFor(f.ctxOrg, subA.SubscriptionID, day(24))
	offersB, _ := f.s.ListMigrationOffersFor(ctxB, subB.SubscriptionID, day(24))
	if len(offersA) != 1 || len(offersB) != 0 || offersA[0].TargetSubscriptionIDs != nil {
		t.Fatalf("offer visibility: A sees %d, B sees %d (and A must not see the target list)", len(offersA), len(offersB))
	}

	accept := domain.ChangeRequest{Kind: domain.ChangeKindMigration, MigrationOfferID: o.MigrationOfferID, PlanAcceptedTermsSHA256: termsHash}
	if _, err := f.s.PreviewChange(ctxB, subB.SubscriptionID, accept, day(24)); !errors.Is(err, domain.ErrNotEligibleForMigration) {
		t.Fatalf("a subscription outside the offer's list was quoted it: %v", err)
	}
	q, err := f.preview(subA, accept, day(24))
	if err != nil {
		t.Fatal(err)
	}
	termEnd := subA.CurrentTerm.EndsAt
	if q.Timing != domain.TimingNextRenewal || !q.EffectiveAt.Equal(termEnd) || q.MigrationOfferID != o.MigrationOfferID {
		t.Fatalf("migration quote: %+v", q)
	}
	subA = f.sv(f.change(subA, accept, q.QuoteSHA256, day(24)))
	subA = f.sv(f.s.Renew(f.ctxOrg, f.seller(subA, termEnd), f.tclaim(f.org, operator, "renew", subA.SubscriptionID)))
	if subA.CurrentTerm.PriceVersionID != v2.PriceVersionID || subA.Effective.ChangeType != domain.ChangePriceMigrated {
		t.Fatalf("after the migration took effect: term=%s change=%s", subA.CurrentTerm.PriceVersionID, subA.Effective.ChangeType)
	}
	changes, _ := f.s.GetChanges(f.ctxOrg, subA.SubscriptionID)
	if len(changes) != 1 || changes[0].RuleID != nil || changes[0].MigrationOfferID == nil || *changes[0].MigrationOfferID != o.MigrationOfferID {
		t.Fatalf("migration evidence must name the offer, not a rule: %+v", changes)
	}

	withdrawn, err := f.s.WithdrawMigrationOffer(f.ctx, o.MigrationOfferID, o.RowVersion, checker, "superseded", day(30),
		f.claim(checker, "withdraw-offer", o.MigrationOfferID))
	if err != nil || withdrawn.Status != domain.MigrationOfferWithdrawn {
		t.Fatalf("withdraw: %v", err)
	}
	if !strings.Contains(o.Reason, "repricing") {
		t.Fatal("offer reason was not kept")
	}
}
