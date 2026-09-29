package store_test

import (
	"errors"
	"testing"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// COM-05 Platform Commercial Billing, part 5d (migration 000015), against
// real Postgres as the NOSUPERUSER NOBYPASSRLS role.

// mustPublishDunningPolicy publishes a policy effective from t0, so every
// test's synthetic timeline (which starts at t0) has one in force.
func (f *subFixture) mustPublishDunningPolicy(version, n1, n2, restrict, suspend int) *domain.DunningPolicyVersion {
	f.t.Helper()
	p := &domain.DunningPolicyVersion{PolicyVersion: version, Notice1AfterDays: n1, Notice2AfterDays: n2,
		RestrictAfterDays: restrict, SuspendAfterDays: suspend, EffectiveFrom: t0, Reason: "test policy",
		CreatedAt: t0, CreatedByPrincipalID: publisher}
	got, err := f.s.PublishDunningPolicy(f.ctx, p, f.claim(publisher, "PublishDunningPolicy", "policy"))
	if err != nil {
		f.t.Fatalf("publish dunning policy: %v", err)
	}
	return got
}

func (f *subFixture) startDunning(invoiceID string) (*domain.DunningCase, error) {
	id := domain.NewCommercialID(domain.PrefixDunningCase)
	return f.s.StartDunning(f.ctx, id, invoiceID, publisher, day(30), f.claim(publisher, "StartDunning", id))
}

// A case binds to the policy version effective when it opened and escalates
// one stage at a time; StopDunning closes it from any open stage without
// deleting it (COM-CTRL-031).
func TestDunning_LifecycleBindsPolicyAndEscalatesOneStageAtATime(t *testing.T) {
	f := newSub(t)
	policy := f.mustPublishDunningPolicy(1, 7, 14, 21, 30)
	f.publishPlan("business", planOpts{autoRenew: true, base: "20.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	// No case yet: GetDunningStateForInvoice returns nil, not an error.
	c, err := f.s.GetDunningStateForInvoice(f.ctxOrg, inv.InvoiceID)
	if err != nil || c != nil {
		t.Fatalf("dunning state before any case: %+v (err=%v)", c, err)
	}

	dc, err := f.startDunning(inv.InvoiceID)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if dc.Status != domain.DunningNotice1 || dc.PolicyVersion != policy.PolicyVersion {
		t.Fatalf("case after start: %+v", dc)
	}
	if n := f.outboxCount(dc.CaseID, "dunning.started"); n != 1 {
		t.Fatalf("dunning.started events: %d", n)
	}

	// A second case cannot open for the same invoice while one is open.
	if _, err := f.startDunning(inv.InvoiceID); !errors.Is(err, domain.ErrDunningCaseAlreadyOpenForInvoice) {
		t.Fatalf("a second open dunning case for the same invoice was allowed: %v", err)
	}

	dc, err = f.s.AdvanceDunning(f.ctx, dc.CaseID, publisher, day(38), f.claim(publisher, "advance-1", dc.CaseID))
	if err != nil || dc.Status != domain.DunningNotice2 {
		t.Fatalf("advance to NOTICE_2: %+v (err=%v)", dc, err)
	}
	dc, err = f.s.AdvanceDunning(f.ctx, dc.CaseID, publisher, day(45), f.claim(publisher, "advance-2", dc.CaseID))
	if err != nil || dc.Status != domain.DunningRestricted {
		t.Fatalf("advance to RESTRICTED: %+v (err=%v)", dc, err)
	}
	dc, err = f.s.AdvanceDunning(f.ctx, dc.CaseID, publisher, day(52), f.claim(publisher, "advance-3", dc.CaseID))
	if err != nil || dc.Status != domain.DunningSuspended {
		t.Fatalf("advance to SUSPENDED: %+v (err=%v)", dc, err)
	}
	if _, err := f.s.AdvanceDunning(f.ctx, dc.CaseID, publisher, day(60), f.claim(publisher, "advance-4", dc.CaseID)); !errors.Is(err, domain.ErrDunningCaseFullyEscalated) {
		t.Fatalf("advanced past SUSPENDED: %v", err)
	}

	stopped, err := f.s.StopDunning(f.ctx, dc.CaseID, publisher, "invoice paid in full", day(61), f.claim(publisher, "stop", dc.CaseID))
	if err != nil || stopped.Status != domain.DunningClosed || stopped.CloseReason == nil {
		t.Fatalf("stop dunning: %+v (err=%v)", stopped, err)
	}
	if n := f.outboxCount(dc.CaseID, "dunning.ended"); n != 1 {
		t.Fatalf("dunning.ended events: %d", n)
	}

	// Stopping an already-closed case is idempotent, not an error.
	again, err := f.s.StopDunning(f.ctx, dc.CaseID, publisher, "different reason", day(62), f.claim(publisher, "stop-again", dc.CaseID))
	if err != nil || again.Status != domain.DunningClosed || *again.CloseReason != "invoice paid in full" {
		t.Fatalf("idempotent stop: %+v (err=%v)", again, err)
	}

	// Once closed, a new case can open for the same invoice again.
	if _, err := f.startDunning(inv.InvoiceID); err != nil {
		t.Fatalf("reopening dunning after the prior case closed: %v", err)
	}
}

// Negative path #34: a policy published after a case opens must not
// retroactively change what that case is bound to.
func TestDunning_LaterPolicyDoesNotRetroactivelyChangeAnOpenCase(t *testing.T) {
	f := newSub(t)
	f.mustPublishDunningPolicy(1, 7, 14, 21, 30)
	f.publishPlan("business", planOpts{autoRenew: true, base: "20.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	dc, err := f.startDunning(inv.InvoiceID)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}

	// A stricter policy takes effect later, after the case already opened.
	newPolicy := &domain.DunningPolicyVersion{PolicyVersion: 2, Notice1AfterDays: 3, Notice2AfterDays: 6,
		RestrictAfterDays: 9, SuspendAfterDays: 12, EffectiveFrom: day(40), Reason: "stricter policy",
		CreatedAt: day(35), CreatedByPrincipalID: publisher}
	if _, err := f.s.PublishDunningPolicy(f.ctx, newPolicy, f.claim(publisher, "PublishDunningPolicy2", "policy2")); err != nil {
		t.Fatalf("publish stricter policy: %v", err)
	}

	// The already-open case still reports policy_version 1.
	got, err := f.s.GetDunningCase(f.ctxOrg, dc.CaseID)
	if err != nil || got.PolicyVersion != 1 {
		t.Fatalf("open case's policy version changed retroactively: %+v (err=%v)", got, err)
	}

	// A NEW case (on a second, independent org/account — the first
	// account's subscription is still live) opened after day 40 binds to
	// the new policy version.
	org2, account2 := f.newAccount("66666666-6666-6666-6666-666666666666", "USD", "GB")
	f.openBillingAccount(org2)
	ctxOrg2 := svcmiddleware.WithTenant(f.ctx, org2)
	p2 := f.startParams(account2, "business", day(60))
	v2, err := f.s.StartSubscription(ctxOrg2, p2, f.tclaim(org2, p2.Actor, "StartSubscription", p2.SubscriptionID))
	if err != nil {
		t.Fatalf("start subscription for org2: %v", err)
	}
	sub2 := f.sv(f.s.ActivateSubscription(ctxOrg2, f.seller(v2, day(60)), false, f.tclaim(org2, operator, "activate", v2.SubscriptionID)))
	inv2 := f.issueSimpleInvoice(org2, sub2.SubscriptionID, sub2.CurrentTerm.TermNo)
	dc2, err := f.s.StartDunning(f.ctx, domain.NewCommercialID(domain.PrefixDunningCase), inv2.InvoiceID, publisher, day(61),
		f.claim(publisher, "StartDunning2", "case2"))
	if err != nil || dc2.PolicyVersion != 2 {
		t.Fatalf("a case opened after the new policy's effective date did not bind to it: %+v (err=%v)", dc2, err)
	}
}

// ReconcileCommercialAccount finds no exceptions on a clean account, and
// GetCommercialReconciliation returns the most recent run.
func TestReconciliation_CleanAccountProducesNoExceptions(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "20.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	if _, err := f.collect(inv.InvoiceID); err != nil {
		t.Fatalf("collect: %v", err)
	}

	id := domain.NewCommercialID(domain.PrefixReconciliation)
	rec, err := f.s.ReconcileCommercialAccount(f.ctx, f.org, publisher, day(40), f.claim(publisher, "reconcile", id))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Status != domain.ReconciliationClean || rec.ExceptionCount != 0 || rec.InvoiceCount != 1 {
		t.Fatalf("reconciliation of a clean account: %+v", rec)
	}
	if n := f.outboxCount(rec.ReconciliationID, "commercial_account.reconciled"); n != 1 {
		t.Fatalf("commercial_account.reconciled events: %d", n)
	}

	got, err := f.s.GetCommercialReconciliation(f.ctxOrg, f.org)
	if err != nil || got.ReconciliationID != rec.ReconciliationID {
		t.Fatalf("get latest reconciliation: %+v (err=%v)", got, err)
	}
}

// Negative path #43: a genuine discrepancy (here, forced via direct SQL
// against the seller plane, simulating something that bypassed the
// application layer) is surfaced as an exception, never hidden.
func TestReconciliation_SurfacesGenuineDiscrepancy(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "20.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	attempt, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: attempt.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("s1"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "settle", attempt.AttemptID)); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Simulate an impossible state a real bug (or manual DB surgery) could
	// cause: a second SUCCEEDED attempt for the same invoice, taking total
	// collected above the invoice total. The unique "one unresolved attempt"
	// index does not block this because both would be resolved (SUCCEEDED),
	// so this genuinely requires reconciliation to catch it.
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	extraID := domain.NewCommercialID(domain.PrefixPaymentAttempt)
	if _, err := tx.Exec(f.ctx, `INSERT INTO payment_attempts (attempt_id, organization_id, invoice_id, amount,
		currency_code, status, created_at, created_by_principal_id, submitted_at, settlement_ref, resolved_at, resolved_by_principal_id)
		VALUES ($1, $2, $3, 20.00, 'USD', 'SUCCEEDED', $4, $5, $4, 's2', $4, $5)`,
		extraID, f.org, inv.InvoiceID, day(25), publisher); err != nil {
		tx.Rollback(f.ctx) //nolint:errcheck
		t.Fatalf("insert forced-duplicate succeeded attempt: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	id := domain.NewCommercialID(domain.PrefixReconciliation)
	rec, err := f.s.ReconcileCommercialAccount(f.ctx, f.org, publisher, day(40), f.claim(publisher, "reconcile-dirty", id))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Status != domain.ReconciliationExceptionsOpen || rec.ExceptionCount == 0 {
		t.Fatalf("an overcollected invoice was not surfaced as a reconciliation exception: %+v", rec)
	}
	found := false
	for _, e := range rec.Exceptions {
		if e.Check == "overcollected" && e.InvoiceID == inv.InvoiceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an overcollected exception for %s, got %+v", inv.InvoiceID, rec.Exceptions)
	}
}

// DB-level negative control: a closed dunning case cannot be mutated
// outside the store's own paths.
func TestDunning_ClosedCaseIsImmutableAtTheDatabase(t *testing.T) {
	f := newSub(t)
	f.mustPublishDunningPolicy(1, 7, 14, 21, 30)
	f.publishPlan("business", planOpts{autoRenew: true, base: "20.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	dc, err := f.startDunning(inv.InvoiceID)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if _, err := f.s.StopDunning(f.ctx, dc.CaseID, publisher, "resolved", day(40), f.claim(publisher, "stop", dc.CaseID)); err != nil {
		t.Fatalf("stop dunning: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dunning_cases SET status = 'NOTICE_1' WHERE case_id = $1`, dc.CaseID); err == nil {
		t.Fatal("a raw UPDATE against a closed dunning case was not rejected")
	}
}
