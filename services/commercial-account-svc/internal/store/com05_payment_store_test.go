package store_test

import (
	"errors"
	"testing"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// COM-05 Platform Commercial Billing, part 5b (migration 000013), against
// real Postgres as the NOSUPERUSER NOBYPASSRLS role.

// issueSimpleInvoice walks GenerateInvoiceCandidate -> ApproveInvoiceCandidate
// -> IssueInvoice for a single RECURRING_FIXED base charge, so payment tests
// don't have to re-derive a rated candidate from scratch.
func (f *subFixture) issueSimpleInvoice(org, subID string, termNo int) *domain.PlatformCommercialInvoice {
	f.t.Helper()
	c, err := f.generateCandidate(org, subID, termNo, "0.00")
	if err != nil {
		f.t.Fatalf("generate candidate: %v", err)
	}
	approved, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21), f.claim(checker, "approve", c.CandidateID))
	if err != nil {
		f.t.Fatalf("approve candidate: %v", err)
	}
	inv, err := f.s.IssueInvoice(f.ctx, approved.CandidateID, publisher, day(22), f.claim(publisher, "issue", approved.CandidateID))
	if err != nil {
		f.t.Fatalf("issue invoice: %v", err)
	}
	return inv
}

func (f *subFixture) collect(invoiceID string) (*domain.PaymentAttemptRef, error) {
	id := domain.NewCommercialID(domain.PrefixPaymentAttempt)
	return f.s.CollectPayment(f.ctx, id, invoiceID, publisher, day(23), f.claim(publisher, "CollectPayment", id))
}

func strp2(s string) *string { return &s }

// Happy path: CollectPayment creates a durable, already-SUBMITTED attempt;
// RecordProviderOutcome(SUCCEEDED) requires settlement evidence
// (COM-CTRL-027) and settles the invoice, reflected in GetCollectionState.
func TestPayment_CollectAndSucceed(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "25.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	state, err := f.s.GetCollectionState(f.ctxOrg, inv.InvoiceID)
	if err != nil || state != domain.CollectionCurrent {
		t.Fatalf("collection state before any attempt: %v (err=%v)", state, err)
	}

	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect payment: %v", err)
	}
	if a.Status != domain.PaymentSubmitted || a.Amount != "25.00" {
		t.Fatalf("attempt after collect: %+v", a)
	}
	if n := f.outboxCount(a.AttemptID, "payment_attempt.created"); n != 1 {
		t.Fatalf("payment_attempt.created events: %d", n)
	}

	state, err = f.s.GetCollectionState(f.ctxOrg, inv.InvoiceID)
	if err != nil || state != domain.CollectionPaymentPending {
		t.Fatalf("collection state while unresolved: %v (err=%v)", state, err)
	}

	// Settlement evidence is mandatory before SUCCEEDED (negative path: no
	// settlement_ref).
	badReq := domain.RecordOutcomeRequest{AttemptID: a.AttemptID, Outcome: domain.PaymentSucceeded,
		OccurredAt: day(24), ActorPrincipalID: publisher}
	if err := badReq.Validate(); err == nil {
		t.Fatal("SUCCEEDED without settlement_ref passed validation")
	}

	settled, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: a.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-1"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "outcome", a.AttemptID))
	if err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if settled.Status != domain.PaymentSucceeded || settled.ResolvedAt == nil {
		t.Fatalf("settled attempt: %+v", settled)
	}
	if n := f.outboxCount(a.AttemptID, "platform_payment.settled"); n != 1 {
		t.Fatalf("platform_payment.settled events: %d", n)
	}

	state, err = f.s.GetCollectionState(f.ctxOrg, inv.InvoiceID)
	if err != nil || state != domain.CollectionPaid {
		t.Fatalf("collection state after settlement: %v (err=%v)", state, err)
	}
}

// COM-CTRL-026 / negative path #27: a second CollectPayment while the first
// is unresolved returns the SAME attempt, never a competing second row.
func TestPayment_NoBlindRetryWhileUnresolved(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "25.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	first, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	second, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if second.AttemptID != first.AttemptID {
		t.Fatalf("a second CollectPayment created a competing attempt: %s vs %s", second.AttemptID, first.AttemptID)
	}
	attempts, err := f.s.GetPaymentAttempts(f.ctxOrg, inv.InvoiceID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts for invoice: %d (err=%v)", len(attempts), err)
	}
}

// CollectPayment after the invoice is already paid is refused outright, not
// silently turned into a second collection.
func TestPayment_CollectAfterAlreadyPaidIsRefused(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "25.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: a.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-2"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "outcome", a.AttemptID)); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if _, err := f.collect(inv.InvoiceID); !errors.Is(err, domain.ErrInvoiceAlreadyPaid) {
		t.Fatalf("collected payment on an already-paid invoice: %v", err)
	}
}

// Negative path #29: out-of-order / regressive outcomes against a resolved
// attempt are refused; an exact replay of the same already-applied outcome
// is answered idempotently instead of erroring.
func TestPayment_ResolvedAttemptRegressionIsRefusedButExactReplayIsIdempotent(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "25.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: a.AttemptID, Outcome: domain.PaymentFailed, FailureReason: strp2("card_declined"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "fail", a.AttemptID)); err != nil {
		t.Fatalf("record failed: %v", err)
	}

	// A late "succeeded" callback arriving after a failure is settled must
	// not flip the attempt.
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: a.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-3"),
		OccurredAt: day(25), ActorPrincipalID: publisher,
	}, f.claim(publisher, "regress", a.AttemptID)); !errors.Is(err, domain.ErrPaymentAttemptInvalidState) {
		t.Fatalf("a resolved attempt regressed from FAILED to SUCCEEDED: %v", err)
	}

	// The exact same FAILED callback replayed (e.g. a duplicate webhook
	// delivery without a distinct provider_event_id) is a no-op, not an error.
	replay, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: a.AttemptID, Outcome: domain.PaymentFailed, FailureReason: strp2("card_declined"),
		OccurredAt: day(26), ActorPrincipalID: publisher,
	}, f.claim(publisher, "replay", a.AttemptID))
	if err != nil || replay.Status != domain.PaymentFailed {
		t.Fatalf("idempotent replay of an already-applied outcome: %+v (err=%v)", replay, err)
	}
}

// Negative path #28: two attempts cannot claim the same provider_event_id —
// deduplicated at the database, not merely by application care.
func TestPayment_DuplicateProviderEventIDIsRejectedAtTheDatabase(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "25.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	invA := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	attemptA, err := f.collect(invA.InvoiceID)
	if err != nil {
		t.Fatalf("collect A: %v", err)
	}
	eventID := "evt-shared-1"
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: attemptA.AttemptID, Outcome: domain.PaymentFailed, FailureReason: strp2("declined"),
		ProviderEventID: &eventID, OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "outcome-a", attemptA.AttemptID)); err != nil {
		t.Fatalf("record outcome A: %v", err)
	}

	// A second, independent org/subscription/invoice/attempt claiming the
	// same provider_event_id must be refused.
	org2, account2 := f.newAccount("55555555-5555-5555-5555-555555555555", "USD", "GB")
	f.openBillingAccount(org2)
	p2 := f.startParams(account2, "business", day(11))
	ctxOrg2 := svcmiddleware.WithTenant(f.ctx, org2)
	v2, err := f.s.StartSubscription(ctxOrg2, p2, f.tclaim(org2, p2.Actor, "StartSubscription", p2.SubscriptionID))
	if err != nil {
		t.Fatalf("start subscription for org2: %v", err)
	}
	sub2 := f.sv(f.s.ActivateSubscription(ctxOrg2, f.seller(v2, day(11)), false, f.tclaim(org2, operator, "activate", v2.SubscriptionID)))
	invB := f.issueSimpleInvoice(org2, sub2.SubscriptionID, sub2.CurrentTerm.TermNo)
	attemptB, err := f.collect(invB.InvoiceID)
	if err != nil {
		t.Fatalf("collect B: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: attemptB.AttemptID, Outcome: domain.PaymentFailed, FailureReason: strp2("declined"),
		ProviderEventID: &eventID, OccurredAt: day(31), ActorPrincipalID: publisher,
	}, f.claim(publisher, "outcome-b", attemptB.AttemptID)); !errors.Is(err, domain.ErrDuplicateProviderEvent) {
		t.Fatalf("a duplicate provider_event_id across attempts was accepted: %v", err)
	}
}

// DB-level negative control: a resolved attempt's row cannot be mutated
// outside the store's own paths.
func TestPayment_ResolvedAttemptIsImmutableAtTheDatabase(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "25.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: a.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-4"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "outcome", a.AttemptID)); err != nil {
		t.Fatalf("record outcome: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE payment_attempts SET status = 'FAILED' WHERE attempt_id = $1`, a.AttemptID); err == nil {
		t.Fatal("a raw UPDATE against a resolved payment attempt was not rejected")
	}
}
