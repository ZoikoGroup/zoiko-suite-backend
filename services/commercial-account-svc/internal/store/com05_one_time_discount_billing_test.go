package store_test

import (
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// COM-05 Platform Commercial Billing, part 5a follow-up (migration 000022):
// ONE_TIME and DISCOUNT components, against real Postgres as the
// NOSUPERUSER NOBYPASSRLS role — the gap RateInvoiceBasis's unit tests
// exercise in isolation, now proven end to end through
// GenerateInvoiceCandidate/ApproveInvoiceCandidate/IssueInvoice, including
// the subscription renewal boundary worker that actually produces term 2
// and term 3.

func oneTimeComponent(key, amount string) domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: key, ComponentType: domain.ComponentOneTime,
		Amount: strp(amount), TriggerEvent: strp("SUBSCRIPTION_START")}
}

func discountComponentDuration(key string, requiresApproval bool, duration int) domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: key, ComponentType: domain.ComponentDiscount, DiscountType: strp("PERCENT"),
		DiscountValue: strp("15"), DurationIntervals: intp(duration), EligibilityCode: strp("ANNUAL_COMMIT"),
		RequiresApproval: boolp(requiresApproval)}
}

func (f *subFixture) termEndsAt(subID string, termNo int) time.Time {
	f.t.Helper()
	var endsAt time.Time
	if err := f.admin.QueryRow(f.ctx, `SELECT ends_at FROM subscription_terms WHERE subscription_id = $1 AND term_no = $2`,
		subID, termNo).Scan(&endsAt); err != nil {
		f.t.Fatalf("read term %d end: %v", termNo, err)
	}
	return endsAt
}

func (f *subFixture) lineOfKind(c *domain.InvoiceCandidate, kind domain.InvoiceLineKind) *domain.InvoiceLine {
	for i := range c.Lines {
		if c.Lines[i].Kind == kind {
			return &c.Lines[i]
		}
	}
	return nil
}

// A one-time setup fee must appear on the first invoice and never appear
// again on a later one for the same subscription — the exact gap this
// service was audited as leaving open: ONE_TIME was valid, bindable, and
// silently never invoiced.
func TestBilling_OneTimeSetupFee_BillsOnceOnFirstInvoiceThenNeverAgain(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "100.00",
		extra: []domain.PriceComponent{oneTimeComponent("setup_fee", "500.00")}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	termEnd := sub.CurrentTerm.EndsAt

	c1, err := f.generateCandidate(f.org, sub.SubscriptionID, 1, "0.00")
	if err != nil {
		t.Fatalf("generate term 1: %v", err)
	}
	if c1.SubtotalAmount != "600.00" {
		t.Fatalf("term 1 subtotal = %s, want 600.00 (100 base + 500 setup fee)", c1.SubtotalAmount)
	}
	setup := f.lineOfKind(c1, domain.LineOneTime)
	if setup == nil || setup.ComponentKey != "setup_fee" {
		t.Fatalf("term 1: no ONE_TIME line in %+v", c1.Lines)
	}
	approved1, err := f.s.ApproveInvoiceCandidate(f.ctx, c1.CandidateID, checker, day(21), f.claim(checker, "approve1", c1.CandidateID))
	if err != nil {
		t.Fatalf("approve term 1: %v", err)
	}
	if _, err := f.s.IssueInvoice(f.ctx, approved1.CandidateID, publisher, day(22), f.claim(publisher, "issue1", approved1.CandidateID)); err != nil {
		t.Fatalf("issue term 1: %v", err)
	}

	// Renew into term 2 through the real boundary worker, not a hand-rolled
	// term row: the fact under test is that GenerateInvoiceCandidate, given
	// this subscription's own issued-invoice history, does not re-bill the
	// fee.
	if n := f.drain(termEnd); n != 1 {
		t.Fatalf("renewal at term end: processed %d, want 1", n)
	}

	c2, err := f.generateCandidate(f.org, sub.SubscriptionID, 2, "0.00")
	if err != nil {
		t.Fatalf("generate term 2: %v", err)
	}
	if c2.SubtotalAmount != "100.00" {
		t.Fatalf("term 2 subtotal = %s, want 100.00 (base only; the setup fee must not re-bill)", c2.SubtotalAmount)
	}
	if got := f.lineOfKind(c2, domain.LineOneTime); got != nil {
		t.Fatalf("term 2 re-billed the one-time setup fee: %+v", got)
	}
	approved2, err := f.s.ApproveInvoiceCandidate(f.ctx, c2.CandidateID, checker, day(52), f.claim(checker, "approve2", c2.CandidateID))
	if err != nil {
		t.Fatalf("approve term 2: %v", err)
	}
	if _, err := f.s.IssueInvoice(f.ctx, approved2.CandidateID, publisher, day(53), f.claim(publisher, "issue2", approved2.CandidateID)); err != nil {
		t.Fatalf("issue term 2: %v", err)
	}
}

// An approved percentage discount reduces the invoice total for exactly its
// duration_intervals window (counted from the term it was approved in,
// across real renewals produced by the boundary worker) and then stops
// applying — without becoming an error.
func TestBilling_PercentDiscount_AppliesForItsWindowThenExpires(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "100.00",
		extra: []domain.PriceComponent{discountComponentDuration("commit_15", true, 2)}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	d, err := f.propose(sub, "business", "commit_15", "sales-sam")
	if err != nil || d.Status != domain.DiscountProposed {
		t.Fatalf("propose: %+v %v", d, err)
	}
	approved, err := f.decide(sub, d, "approve", "finance-fiona", "")
	if err != nil || approved.Status != domain.DiscountApproved {
		t.Fatalf("approve discount: %+v %v", approved, err)
	}

	term1End := f.termEndsAt(sub.SubscriptionID, 1)

	c1, err := f.generateCandidate(f.org, sub.SubscriptionID, 1, "0.00")
	if err != nil {
		t.Fatalf("generate term 1: %v", err)
	}
	if c1.SubtotalAmount != "85.00" {
		t.Fatalf("term 1 subtotal = %s, want 85.00 (100 - 15%% discount)", c1.SubtotalAmount)
	}
	disc1 := f.lineOfKind(c1, domain.LineDiscount)
	if disc1 == nil || disc1.DiscountApplicationID == nil || *disc1.DiscountApplicationID != approved.DiscountApplicationID {
		t.Fatalf("term 1 discount lineage: %+v", disc1)
	}
	a1, err := f.s.ApproveInvoiceCandidate(f.ctx, c1.CandidateID, checker, day(21), f.claim(checker, "approve-c1", c1.CandidateID))
	if err != nil {
		t.Fatalf("approve c1: %v", err)
	}
	if _, err := f.s.IssueInvoice(f.ctx, a1.CandidateID, publisher, day(22), f.claim(publisher, "issue-c1", a1.CandidateID)); err != nil {
		t.Fatalf("issue c1: %v", err)
	}

	if n := f.drain(term1End); n != 1 {
		t.Fatalf("renewal into term 2: processed %d, want 1", n)
	}
	term2End := f.termEndsAt(sub.SubscriptionID, 2)

	// Term 2 is still inside the 2-interval window.
	c2, err := f.generateCandidate(f.org, sub.SubscriptionID, 2, "0.00")
	if err != nil {
		t.Fatalf("generate term 2: %v", err)
	}
	if c2.SubtotalAmount != "85.00" {
		t.Fatalf("term 2 subtotal = %s, want 85.00 (still within the discount's window)", c2.SubtotalAmount)
	}
	a2, err := f.s.ApproveInvoiceCandidate(f.ctx, c2.CandidateID, checker, day(52), f.claim(checker, "approve-c2", c2.CandidateID))
	if err != nil {
		t.Fatalf("approve c2: %v", err)
	}
	if _, err := f.s.IssueInvoice(f.ctx, a2.CandidateID, publisher, day(53), f.claim(publisher, "issue-c2", a2.CandidateID)); err != nil {
		t.Fatalf("issue c2: %v", err)
	}

	if n := f.drain(term2End); n != 1 {
		t.Fatalf("renewal into term 3: processed %d, want 1", n)
	}

	// Term 3 is outside the 2-interval window: the discount must no longer
	// apply, and this must not be an error.
	c3, err := f.generateCandidate(f.org, sub.SubscriptionID, 3, "0.00")
	if err != nil {
		t.Fatalf("generate term 3: %v", err)
	}
	if c3.SubtotalAmount != "100.00" {
		t.Fatalf("term 3 subtotal = %s, want 100.00 (the discount's window has passed)", c3.SubtotalAmount)
	}
	if got := f.lineOfKind(c3, domain.LineDiscount); got != nil {
		t.Fatalf("term 3 still carries the expired discount: %+v", got)
	}
}

// A discount that requires approval and has never been decided (still
// PROPOSED) must never be applied — refused by omission, not by error: it
// simply does not appear as a line, and the invoice rates as if it did not
// exist. COM-CTRL-005's delegated-authority doctrine would be defeated if
// an unapproved discount could still reach an invoice.
func TestBilling_DiscountRequiringApproval_IsNotAppliedWhileUnapproved(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "50.00",
		extra: []domain.PriceComponent{discountComponentDuration("commit_15", true, 3)}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	d, err := f.propose(sub, "business", "commit_15", "sales-sam")
	if err != nil || d.Status != domain.DiscountProposed {
		t.Fatalf("propose: %+v %v", d, err)
	}
	// Deliberately not decided: still PROPOSED.

	c, err := f.generateCandidate(f.org, sub.SubscriptionID, 1, "0.00")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if c.SubtotalAmount != "50.00" {
		t.Fatalf("subtotal = %s, want 50.00 (an unapproved discount must not reduce it)", c.SubtotalAmount)
	}
	if got := f.lineOfKind(c, domain.LineDiscount); got != nil {
		t.Fatalf("an unapproved discount produced a line: %+v", got)
	}
	approved, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21), f.claim(checker, "approve", c.CandidateID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := f.s.IssueInvoice(f.ctx, approved.CandidateID, publisher, day(22), f.claim(publisher, "issue", approved.CandidateID)); err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A rejected discount must stay unapplied too, on the very next term.
	rejected, err := f.decide(sub, d, "reject", "finance-fiona", "not qualified")
	if err != nil || rejected.Status != domain.DiscountRejected {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if n := f.drain(sub.CurrentTerm.EndsAt); n != 1 {
		t.Fatalf("renewal into term 2: processed %d, want 1", n)
	}
	c2, err := f.generateCandidate(f.org, sub.SubscriptionID, 2, "0.00")
	if err != nil {
		t.Fatalf("generate term 2: %v", err)
	}
	if c2.SubtotalAmount != "50.00" {
		t.Fatalf("term 2 subtotal = %s, want 50.00 (a rejected discount must not reduce it)", c2.SubtotalAmount)
	}
	if got := f.lineOfKind(c2, domain.LineDiscount); got != nil {
		t.Fatalf("a rejected discount produced a line: %+v", got)
	}
}
