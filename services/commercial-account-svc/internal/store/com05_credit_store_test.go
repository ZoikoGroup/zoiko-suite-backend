package store_test

import (
	"errors"
	"testing"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// COM-05 Platform Commercial Billing, part 5c (migration 000014), against
// real Postgres as the NOSUPERUSER NOBYPASSRLS role.

func (f *subFixture) issueCreditNote(invoiceID, amount, reason string) (*domain.CreditNote, error) {
	id := domain.NewCommercialID(domain.PrefixCreditNote)
	return f.s.IssueCreditNote(f.ctx, id, invoiceID, amount, reason, publisher, day(25), f.claim(publisher, "IssueCreditNote", id))
}

func (f *subFixture) applyWriteOff(invoiceID, amount, reason string) (*domain.WriteOff, error) {
	id := domain.NewCommercialID(domain.PrefixWriteOff)
	return f.s.ApplyWriteOff(f.ctx, id, invoiceID, amount, reason, publisher, day(25), f.claim(publisher, "ApplyWriteOff", id))
}

// A credit note reduces what is owed; GetBalance reflects it exactly.
func TestCredit_IssueCreditNoteReducesBalance(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "100.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	bal, err := f.s.GetBalance(f.ctxOrg, f.org)
	if err != nil || bal.Balance != "100.00" {
		t.Fatalf("balance before credit: %+v (err=%v)", bal, err)
	}

	c, err := f.issueCreditNote(inv.InvoiceID, "30.00", "goodwill adjustment")
	if err != nil {
		t.Fatalf("issue credit note: %v", err)
	}
	if c.Amount != "30.00" {
		t.Fatalf("credit note amount: %+v", c)
	}
	if n := f.outboxCount(c.CreditNoteID, "credit_note.issued"); n != 1 {
		t.Fatalf("credit_note.issued events: %d", n)
	}

	bal, err = f.s.GetBalance(f.ctxOrg, f.org)
	if err != nil || bal.Balance != "70.00" || bal.TotalCredited != "30.00" {
		t.Fatalf("balance after credit: %+v (err=%v)", bal, err)
	}

	// Crediting more than remains owed is refused.
	if _, err := f.issueCreditNote(inv.InvoiceID, "71.00", "too much"); !errors.Is(err, domain.ErrCreditExceedsInvoice) {
		t.Fatalf("a credit note exceeding the invoice's remaining balance was accepted: %v", err)
	}
	// Exactly the remainder is fine.
	if _, err := f.issueCreditNote(inv.InvoiceID, "70.00", "the rest"); err != nil {
		t.Fatalf("crediting exactly the remainder: %v", err)
	}
	bal, err = f.s.GetBalance(f.ctxOrg, f.org)
	if err != nil || bal.Balance != "0.00" {
		t.Fatalf("balance after crediting in full: %+v (err=%v)", bal, err)
	}
}

// A write-off shares the same "amount owed" pool as credit notes: the two
// together can never exceed the invoice total.
func TestCredit_WriteOffSharesPoolWithCreditNotes(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "50.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	if _, err := f.issueCreditNote(inv.InvoiceID, "20.00", "partial credit"); err != nil {
		t.Fatalf("credit: %v", err)
	}
	if _, err := f.applyWriteOff(inv.InvoiceID, "30.00", "uncollectible remainder"); err != nil {
		t.Fatalf("write-off of exactly the remainder: %v", err)
	}
	if _, err := f.applyWriteOff(inv.InvoiceID, "0.01", "one cent too many"); !errors.Is(err, domain.ErrWriteOffExceedsInvoice) {
		t.Fatalf("a write-off exceeding the combined pool was accepted: %v", err)
	}
	bal, err := f.s.GetBalance(f.ctxOrg, f.org)
	if err != nil || bal.Balance != "0.00" || bal.TotalWrittenOff != "30.00" {
		t.Fatalf("balance: %+v (err=%v)", bal, err)
	}
}

// Full lifecycle: collect, succeed, then refund — COM-CTRL-028's destination
// fingerprint must match at settlement, and the refund reduces net
// collected in GetBalance.
func TestCredit_RefundLifecycleAndDestinationFingerprint(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "40.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	attempt, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	// A refund cannot be requested before the attempt has succeeded.
	rid0 := domain.NewCommercialID(domain.PrefixRefundRequest)
	if _, err := f.s.RequestRefund(f.ctx, rid0, inv.InvoiceID, attempt.AttemptID, "10.00", "card:tok_abc", "too early",
		publisher, day(23), f.claim(publisher, "refund-early", rid0)); !errors.Is(err, domain.ErrPaymentAttemptNotSucceeded) {
		t.Fatalf("a refund was requested against an unresolved attempt: %v", err)
	}

	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: attempt.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-x"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "settle", attempt.AttemptID)); err != nil {
		t.Fatalf("settle payment: %v", err)
	}

	rid := domain.NewCommercialID(domain.PrefixRefundRequest)
	rr, err := f.s.RequestRefund(f.ctx, rid, inv.InvoiceID, attempt.AttemptID, "15.00", "card:tok_abc", "customer requested",
		publisher, day(25), f.claim(publisher, "refund", rid))
	if err != nil {
		t.Fatalf("request refund: %v", err)
	}
	if rr.Status != domain.RefundRequested {
		t.Fatalf("refund status: %+v", rr)
	}
	if n := f.outboxCount(rr.RefundID, "refund.requested"); n != 1 {
		t.Fatalf("refund.requested events: %d", n)
	}

	// Settling against a DIFFERENT destination than requested is refused
	// (negative path #31).
	if _, err := f.s.SettleRefund(f.ctx, domain.SettleRefundRequest{
		RefundID: rr.RefundID, Outcome: domain.RefundSettled, DestinationRef: "card:tok_XYZ",
		SettlementRef: strp2("refund-settle-1"), OccurredAt: day(26), ActorPrincipalID: publisher,
	}, f.claim(publisher, "settle-wrong-dest", rr.RefundID)); !errors.Is(err, domain.ErrRefundDestinationMismatch) {
		t.Fatalf("a refund settled against a changed destination: %v", err)
	}

	settled, err := f.s.SettleRefund(f.ctx, domain.SettleRefundRequest{
		RefundID: rr.RefundID, Outcome: domain.RefundSettled, DestinationRef: "card:tok_abc",
		SettlementRef: strp2("refund-settle-1"), OccurredAt: day(26), ActorPrincipalID: publisher,
	}, f.claim(publisher, "settle-refund", rr.RefundID))
	if err != nil || settled.Status != domain.RefundSettled {
		t.Fatalf("settle refund: %+v (err=%v)", settled, err)
	}
	if n := f.outboxCount(rr.RefundID, "refund.settled"); n != 1 {
		t.Fatalf("refund.settled events: %d", n)
	}

	// Requesting more than remains collected on this attempt is refused.
	rid2 := domain.NewCommercialID(domain.PrefixRefundRequest)
	if _, err := f.s.RequestRefund(f.ctx, rid2, inv.InvoiceID, attempt.AttemptID, "30.00", "card:tok_abc", "too much",
		publisher, day(27), f.claim(publisher, "refund-too-much", rid2)); !errors.Is(err, domain.ErrRefundExceedsCollected) {
		t.Fatalf("a refund exceeding the remaining collected amount was accepted: %v", err)
	}

	bal, err := f.s.GetBalance(f.ctxOrg, f.org)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	// 40.00 invoiced, 40.00 collected, 15.00 refunded -> net collected 25.00,
	// balance = 40.00 - 25.00 = 15.00.
	if bal.Balance != "15.00" || bal.TotalRefunded != "15.00" {
		t.Fatalf("balance after refund: %+v", bal)
	}
}

// DB-level negative control: a settled refund's row cannot be mutated
// outside the store's own paths.
func TestCredit_SettledRefundIsImmutableAtTheDatabase(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "40.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
	attempt, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{
		AttemptID: attempt.AttemptID, Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-y"),
		OccurredAt: day(24), ActorPrincipalID: publisher,
	}, f.claim(publisher, "settle", attempt.AttemptID)); err != nil {
		t.Fatalf("settle payment: %v", err)
	}
	rid := domain.NewCommercialID(domain.PrefixRefundRequest)
	rr, err := f.s.RequestRefund(f.ctx, rid, inv.InvoiceID, attempt.AttemptID, "5.00", "card:tok_abc", "test",
		publisher, day(25), f.claim(publisher, "refund", rid))
	if err != nil {
		t.Fatalf("request refund: %v", err)
	}
	if _, err := f.s.SettleRefund(f.ctx, domain.SettleRefundRequest{
		RefundID: rr.RefundID, Outcome: domain.RefundSettled, DestinationRef: "card:tok_abc",
		SettlementRef: strp2("refund-settle-2"), OccurredAt: day(26), ActorPrincipalID: publisher,
	}, f.claim(publisher, "settle-refund", rr.RefundID)); err != nil {
		t.Fatalf("settle refund: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE refund_requests SET status = 'FAILED' WHERE refund_id = $1`, rr.RefundID); err == nil {
		t.Fatal("a raw UPDATE against a settled refund request was not rejected")
	}
}
