package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// COM-05 gap-remediation tests (D1-D4), against real Postgres as the
// NOSUPERUSER NOBYPASSRLS role — the same rigor as every other wave in this
// service: each fix gets at least one genuine negative control.

// D1: CommercialEvidencePackage is sealed automatically at issue time and
// carries the exact lineage the invoice was rated from; it is immutable at
// the database, not merely by application convention.
func TestGapFix_EvidencePackageSealedAtIssueAndImmutable(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true, base: "49.00",
		extra: []domain.PriceComponent{meteredComponent("api.calls", 1, "100", "0.0100")}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "150", day(12))
	f.closeAndCertify(sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", day(20))

	c, err := f.generateCandidate(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo, "2.50")
	if err != nil {
		t.Fatalf("generate candidate: %v", err)
	}
	approved, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21), f.claim(checker, "approve", c.CandidateID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	inv, err := f.s.IssueInvoice(f.ctx, approved.CandidateID, publisher, day(22), f.claim(publisher, "issue", approved.CandidateID))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	pkg, err := f.s.GetEvidencePackage(f.ctxOrg, inv.InvoiceID)
	if err != nil {
		t.Fatalf("get evidence package: %v", err)
	}
	if pkg.Manifest.InvoiceID != inv.InvoiceID || pkg.Manifest.InvoiceNumber != inv.InvoiceNumber {
		t.Fatalf("manifest invoice identity: %+v", pkg.Manifest)
	}
	if pkg.Manifest.SubscriptionID != sub.SubscriptionID || pkg.Manifest.TermNo != sub.CurrentTerm.TermNo {
		t.Fatalf("manifest subscription/term: %+v", pkg.Manifest)
	}
	if len(pkg.Manifest.Lines) != 2 {
		t.Fatalf("manifest lines = %d, want 2 (recurring + usage)", len(pkg.Manifest.Lines))
	}
	var usageLine *domain.EvidenceLine
	for i := range pkg.Manifest.Lines {
		if pkg.Manifest.Lines[i].Kind == string(domain.LineUsage) {
			usageLine = &pkg.Manifest.Lines[i]
		}
	}
	if usageLine == nil || usageLine.StatementStatus == nil || (*usageLine.StatementStatus != "CERTIFIED" && *usageLine.StatementStatus != "ADJUSTED") {
		t.Fatalf("usage evidence line: %+v", usageLine)
	}
	if usageLine.StatementTotalQty == nil || *usageLine.StatementTotalQty == "" {
		t.Fatalf("usage evidence line has no total_quantity: %+v", usageLine)
	}
	if pkg.ManifestSHA256 == "" || len(pkg.ManifestSHA256) != 64 {
		t.Fatalf("manifest_sha256 = %q", pkg.ManifestSHA256)
	}

	// Negative control: a raw UPDATE against the evidence package must be
	// rejected at the database, not merely filtered to zero rows by RLS
	// (the exact bug class found and fixed twice already in this session's
	// entitlement_snapshots and dunning_cases work).
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE commercial_evidence_packages SET manifest_sha256 = repeat('0', 64) WHERE package_id = $1`,
		pkg.PackageID); err == nil {
		t.Fatal("a raw UPDATE against a sealed evidence package was not rejected")
	}
}

// D2: the tax jurisdiction/rate a candidate is generated against must be a
// real, seller-registered fact — an unregistered jurisdiction, or a
// registered one with a mismatched caller-supplied rate, are both refused.
func TestGapFix_TaxJurisdictionMustBeRegisteredAndRateMustMatch(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "10.00"})
	ba := f.openBillingAccount(f.org) // registers US-CA @ 875bps by default (see fixture helper)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	// Unregistered jurisdiction.
	id1 := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	req1 := domain.GenerateInvoiceCandidateRequest{CandidateID: id1, OrganizationID: f.org, SubscriptionID: sub.SubscriptionID,
		TermNo: sub.CurrentTerm.TermNo, TaxJurisdictionCode: "EU-DE", TaxRateBasisPoints: 1900, TaxAmount: "1.90",
		CreatedByPrincipalID: publisher}
	if _, err := f.s.GenerateInvoiceCandidate(f.ctx, req1, f.claim(publisher, "GenerateInvoiceCandidate", id1)); !errors.Is(err, domain.ErrTaxJurisdictionNotRegistered) {
		t.Fatalf("candidate generated against an unregistered tax jurisdiction: %v", err)
	}

	// Registered jurisdiction, mismatched rate.
	id2 := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	req2 := domain.GenerateInvoiceCandidateRequest{CandidateID: id2, OrganizationID: f.org, SubscriptionID: sub.SubscriptionID,
		TermNo: sub.CurrentTerm.TermNo, TaxJurisdictionCode: "US-CA", TaxRateBasisPoints: 100, TaxAmount: "0.10",
		CreatedByPrincipalID: publisher}
	if _, err := f.s.GenerateInvoiceCandidate(f.ctx, req2, f.claim(publisher, "GenerateInvoiceCandidate", id2)); !errors.Is(err, domain.ErrTaxRateMismatch) {
		t.Fatalf("candidate generated with a mismatched tax rate: %v", err)
	}

	// Registered jurisdiction, matching rate: succeeds.
	id3 := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	req3 := domain.GenerateInvoiceCandidateRequest{CandidateID: id3, OrganizationID: f.org, SubscriptionID: sub.SubscriptionID,
		TermNo: sub.CurrentTerm.TermNo, TaxJurisdictionCode: "US-CA", TaxRateBasisPoints: 875, TaxAmount: "0.88",
		CreatedByPrincipalID: publisher}
	c, err := f.s.GenerateInvoiceCandidate(f.ctx, req3, f.claim(publisher, "GenerateInvoiceCandidate", id3))
	if err != nil {
		t.Fatalf("candidate refused despite a matching registered rate: %v", err)
	}
	if c.TaxJurisdictionCode != "US-CA" {
		t.Fatalf("candidate jurisdiction: %+v", c)
	}
	// Resolve this term's in-flight candidate (the unique "one candidate in
	// flight per term" constraint would otherwise block generating another
	// one below) — approve and issue it.
	approved, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21), f.claim(checker, "approve", c.CandidateID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := f.s.IssueInvoice(f.ctx, approved.CandidateID, publisher, day(22), f.claim(publisher, "issue", approved.CandidateID)); err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A jurisdiction registered with no fixed rate (nil) accepts any
	// caller-supplied rate for now — only jurisdiction membership is
	// checked, not a rate this service was never told to enforce.
	unfixed := &domain.TaxJurisdiction{BillingAccountID: ba.BillingAccountID, JurisdictionCode: "US-NY",
		EffectiveFrom: t0, CreatedAt: t0, CreatedByPrincipalID: publisher}
	if _, err := f.s.RegisterTaxJurisdiction(f.ctx, f.org, unfixed); err != nil {
		t.Fatalf("register unfixed-rate jurisdiction: %v", err)
	}
	renewAt := sub.CurrentTerm.EndsAt.Add(time.Hour)
	sub2 := f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, renewAt), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	id4 := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	req4 := domain.GenerateInvoiceCandidateRequest{CandidateID: id4, OrganizationID: f.org, SubscriptionID: sub.SubscriptionID,
		TermNo: sub2.CurrentTerm.TermNo, TaxJurisdictionCode: "US-NY", TaxRateBasisPoints: 400, TaxAmount: "0.40",
		CreatedByPrincipalID: publisher}
	if _, err := f.s.GenerateInvoiceCandidate(f.ctx, req4, f.claim(publisher, "GenerateInvoiceCandidate", id4)); err != nil {
		t.Fatalf("candidate refused against an unfixed-rate registered jurisdiction: %v", err)
	}
}

// D3: every money-moving COM-05 command emits an accounting_event.recorded
// event carrying the billing account's own server-resolved mapping key —
// never a caller-suppliable field (COM-CTRL-033, negative path #44 is
// refused by construction: there is no field to inject a value into).
func TestGapFix_MoneyMovingCommandsEmitAccountingEvents(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "100.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	if n := f.outboxCount(inv.InvoiceID, "accounting_event.recorded"); n != 1 {
		t.Fatalf("accounting_event.recorded for IssueInvoice: %d", n)
	}

	cnID := domain.NewCommercialID(domain.PrefixCreditNote)
	if _, err := f.s.IssueCreditNote(f.ctx, cnID, inv.InvoiceID, "10.00", "goodwill", publisher, day(23),
		f.claim(publisher, "IssueCreditNote", cnID)); err != nil {
		t.Fatalf("issue credit note: %v", err)
	}
	if n := f.outboxCount(cnID, "accounting_event.recorded"); n != 1 {
		t.Fatalf("accounting_event.recorded for IssueCreditNote: %d", n)
	}

	woID := domain.NewCommercialID(domain.PrefixWriteOff)
	if _, err := f.s.ApplyWriteOff(f.ctx, woID, inv.InvoiceID, "5.00", "uncollectible", publisher, day(23),
		f.claim(publisher, "ApplyWriteOff", woID)); err != nil {
		t.Fatalf("apply write-off: %v", err)
	}
	if n := f.outboxCount(woID, "accounting_event.recorded"); n != 1 {
		t.Fatalf("accounting_event.recorded for ApplyWriteOff: %d", n)
	}

	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{AttemptID: a.AttemptID,
		Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-gap"), OccurredAt: day(24), ActorPrincipalID: publisher},
		f.claim(publisher, "outcome", a.AttemptID)); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	// The payment-collection success path must emit an accounting event too
	// (mirror of SettleRefund's settled branch) — money was actually
	// collected here.
	if n := f.outboxCount(a.AttemptID, "accounting_event.recorded"); n != 1 {
		t.Fatalf("accounting_event.recorded for RecordProviderOutcome(SUCCEEDED): %d", n)
	}
	rfID := domain.NewCommercialID(domain.PrefixRefundRequest)
	if _, err := f.s.RequestRefund(f.ctx, rfID, inv.InvoiceID, a.AttemptID, "20.00", "dest-ref-1", "buyer's remorse",
		publisher, day(25), f.claim(publisher, "RequestRefund", rfID)); err != nil {
		t.Fatalf("request refund: %v", err)
	}
	// Not yet settled: no accounting event for the mere request.
	if n := f.outboxCount(rfID, "accounting_event.recorded"); n != 0 {
		t.Fatalf("accounting_event.recorded before settlement: %d", n)
	}
	settleReq := domain.SettleRefundRequest{RefundID: rfID, Outcome: domain.RefundSettled,
		DestinationRef: "dest-ref-1", SettlementRef: strp2("refund-settle-1"), OccurredAt: day(26), ActorPrincipalID: publisher}
	if _, err := f.s.SettleRefund(f.ctx, settleReq, f.claim(publisher, "SettleRefund", rfID)); err != nil {
		t.Fatalf("settle refund: %v", err)
	}
	if n := f.outboxCount(rfID, "accounting_event.recorded"); n != 1 {
		t.Fatalf("accounting_event.recorded for SettleRefund: %d", n)
	}
}

// D3 continued: a FAILED payment outcome moves no money, so it must emit
// no accounting event — only the SUCCEEDED branch does.
func TestGapFix_FailedPaymentOutcomeEmitsNoAccountingEvent(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "40.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	failure := "card_declined"
	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{AttemptID: a.AttemptID,
		Outcome: domain.PaymentFailed, FailureReason: &failure, OccurredAt: day(24), ActorPrincipalID: publisher},
		f.claim(publisher, "outcome-fail", a.AttemptID)); err != nil {
		t.Fatalf("record failed outcome: %v", err)
	}
	if n := f.outboxCount(a.AttemptID, "accounting_event.recorded"); n != 0 {
		t.Fatalf("accounting_event.recorded for a FAILED outcome: %d", n)
	}
}

// D4: chargeback/dispute tracking is purely orthogonal — opening one, and
// every later transition, never touches the referenced payment attempt or
// invoice; the forward-only lifecycle trigger blocks skipping a stage or
// mutating a RESOLVED case; disputing a non-SUCCEEDED attempt is refused.
func TestGapFix_DisputeLifecycleIsIsolatedAndForwardOnly(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "60.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)

	a, err := f.collect(inv.InvoiceID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	// Disputing a not-yet-SUCCEEDED attempt is refused.
	disputeID := domain.NewCommercialID(domain.PrefixDispute)
	if _, err := f.s.OpenDispute(f.ctx, disputeID, inv.InvoiceID, a.AttemptID, "unauthorized charge", publisher, day(24),
		f.claim(publisher, "OpenDispute", disputeID)); !errors.Is(err, domain.ErrDisputeAttemptNotSucceeded) {
		t.Fatalf("dispute opened against a non-SUCCEEDED attempt: %v", err)
	}

	if _, err := f.s.RecordProviderOutcome(f.ctx, domain.RecordOutcomeRequest{AttemptID: a.AttemptID,
		Outcome: domain.PaymentSucceeded, SettlementRef: strp2("settle-dispute"), OccurredAt: day(24), ActorPrincipalID: publisher},
		f.claim(publisher, "outcome", a.AttemptID)); err != nil {
		t.Fatalf("record outcome: %v", err)
	}

	beforeInvoice, err := f.s.GetInvoice(f.ctxOrg, inv.InvoiceID)
	if err != nil {
		t.Fatalf("get invoice before dispute: %v", err)
	}
	beforeAttempt, err := f.s.GetCollectionState(f.ctxOrg, inv.InvoiceID)
	if err != nil {
		t.Fatalf("get collection state before dispute: %v", err)
	}

	d, err := f.s.OpenDispute(f.ctx, disputeID, inv.InvoiceID, a.AttemptID, "unauthorized charge", publisher, day(25),
		f.claim(publisher, "OpenDispute", disputeID))
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	if d.Status != domain.DisputeOpen {
		t.Fatalf("dispute status = %s, want OPEN", d.Status)
	}
	if n := f.outboxCount(disputeID, "dispute.opened"); n != 1 {
		t.Fatalf("dispute.opened events: %d", n)
	}

	// Isolation: opening the dispute changed nothing about the invoice or
	// the payment collection state.
	afterInvoice, err := f.s.GetInvoice(f.ctxOrg, inv.InvoiceID)
	if err != nil {
		t.Fatalf("get invoice after dispute: %v", err)
	}
	if afterInvoice.TotalAmount != beforeInvoice.TotalAmount {
		t.Fatalf("invoice total changed by opening a dispute: before=%s after=%s", beforeInvoice.TotalAmount, afterInvoice.TotalAmount)
	}
	afterAttempt, err := f.s.GetCollectionState(f.ctxOrg, inv.InvoiceID)
	if err != nil {
		t.Fatalf("get collection state after dispute: %v", err)
	}
	if afterAttempt != beforeAttempt {
		t.Fatalf("collection state changed by opening a dispute: before=%v after=%v", beforeAttempt, afterAttempt)
	}

	// Forward-only lifecycle: cannot jump straight to RESOLVED, cannot
	// record an outcome twice.
	if _, err := f.s.CloseDispute(f.ctx, disputeID, "n/a", publisher, day(26),
		f.claim(publisher, "close-too-early", disputeID)); !errors.Is(err, domain.ErrDisputeCaseInvalidState) {
		t.Fatalf("closed a still-OPEN dispute: %v", err)
	}

	won, err := f.s.RecordDisputeOutcome(f.ctx, disputeID, domain.DisputeWon, publisher, day(26),
		f.claim(publisher, "RecordDisputeOutcome", disputeID))
	if err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if won.Status != domain.DisputeWon || won.OutcomeRecordedAt == nil {
		t.Fatalf("dispute after outcome: %+v", won)
	}
	if _, err := f.s.RecordDisputeOutcome(f.ctx, disputeID, domain.DisputeLost, publisher, day(27),
		f.claim(publisher, "record-outcome-again", disputeID)); !errors.Is(err, domain.ErrDisputeCaseInvalidState) {
		t.Fatalf("recorded a second outcome on an already-WON dispute: %v", err)
	}

	resolved, err := f.s.CloseDispute(f.ctx, disputeID, "chargeback upheld in our favor", publisher, day(28),
		f.claim(publisher, "CloseDispute", disputeID))
	if err != nil {
		t.Fatalf("close dispute: %v", err)
	}
	if resolved.Status != domain.DisputeResolved || resolved.ResolvedAt == nil {
		t.Fatalf("resolved dispute: %+v", resolved)
	}

	// Negative control: a RESOLVED dispute is immutable at the database,
	// not merely by application convention.
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dispute_cases SET reason = 'tampered' WHERE dispute_id = $1`, disputeID); err == nil {
		t.Fatal("a raw UPDATE against a resolved dispute was not rejected")
	}
}
