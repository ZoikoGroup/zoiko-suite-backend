package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// COM-05 Platform Commercial Billing, part 5a (migration 000012), against
// real Postgres as the NOSUPERUSER NOBYPASSRLS role.

func meteredComponent(meterKey string, version int, included, unitAmount string) domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: "api_calls", ComponentType: domain.ComponentMetered,
		MeterKey: strp(meterKey), MeterVersion: intp(version), AggregationMethod: strp("SUM"),
		IncludedQuantity: strp(included), BillingTiming: strp("IN_ARREARS"), Amount: strp(unitAmount)}
}

func (f *subFixture) openBillingAccount(org string) *domain.BillingAccount {
	f.t.Helper()
	b := &domain.BillingAccount{
		BillingAccountID: domain.NewCommercialID(domain.PrefixBillingAccount), OrganizationID: org,
		SellingEntity: "ZOIKOSUITE_INC_US", BillingCurrencyCode: "USD", InvoiceNumberingProfile: "US-STD",
		PaymentProviderRef: "provider:stripe:acct_test", AccountingMappingKey: "gl:us-inc:default",
		CreatedAt: t0, CreatedByPrincipalID: publisher,
	}
	got, err := f.s.OpenBillingAccount(f.ctx, b, f.claim(publisher, "OpenBillingAccount", org))
	if err != nil {
		f.t.Fatalf("open billing account: %v", err)
	}
	// Every existing test generates candidates against the "US-CA"/875bps
	// jurisdiction (below) — registering it here by default keeps every
	// pre-existing caller of generateCandidate working under D2's new
	// server-side registration requirement without touching each test.
	rate := 875
	j := &domain.TaxJurisdiction{
		BillingAccountID: got.BillingAccountID, JurisdictionCode: "US-CA", RegisteredRateBasisPoints: &rate,
		EffectiveFrom: t0, CreatedAt: t0, CreatedByPrincipalID: publisher,
	}
	if _, err := f.s.RegisterTaxJurisdiction(f.ctx, org, j); err != nil {
		f.t.Fatalf("register tax jurisdiction: %v", err)
	}
	return got
}

func (f *subFixture) generateCandidate(org, subID string, termNo int, taxAmount string) (*domain.InvoiceCandidate, error) {
	id := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	req := domain.GenerateInvoiceCandidateRequest{
		CandidateID: id, OrganizationID: org, SubscriptionID: subID, TermNo: termNo,
		TaxJurisdictionCode: "US-CA", TaxRateBasisPoints: 875, TaxAmount: taxAmount, CreatedByPrincipalID: publisher,
	}
	return f.s.GenerateInvoiceCandidate(f.ctx, req, f.claim(publisher, "GenerateInvoiceCandidate", id))
}

// Charge Basis (§5): the candidate's lines are exactly the RECURRING_FIXED
// base charge plus the METERED component rated against the term's certified
// usage statement, and total = subtotal + tax exactly (DB CHECK, never a
// second independent rounding).
func TestBilling_GenerateApproveIssue_RatesRecurringAndUsage(t *testing.T) {
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
	if c.Status != domain.CandidateDraft {
		t.Fatalf("status = %s, want DRAFT", c.Status)
	}
	if len(c.Lines) != 2 {
		t.Fatalf("lines = %d, want 2 (recurring + usage)", len(c.Lines))
	}
	// 100 included of 150 billable -> 50 * 0.01 = 0.50; subtotal = 49.00 + 0.50.
	if c.SubtotalAmount != "49.50" {
		t.Fatalf("subtotal = %s, want 49.50", c.SubtotalAmount)
	}
	if c.TotalAmount != "52.00" {
		t.Fatalf("total = %s, want 52.00 (subtotal + tax exactly)", c.TotalAmount)
	}
	var usageLine *domain.InvoiceLine
	for i := range c.Lines {
		if c.Lines[i].Kind == domain.LineUsage {
			usageLine = &c.Lines[i]
		}
	}
	if usageLine == nil || usageLine.Amount != "0.50000000" || usageLine.MeterKey == nil || *usageLine.MeterKey != "api.calls" {
		t.Fatalf("usage line: %+v", usageLine)
	}

	// Self-approval is refused (SoD, same doctrine as every other wave).
	if _, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, publisher, day(21),
		f.claim(publisher, "approve-self", c.CandidateID)); !errors.Is(err, domain.ErrInvoiceCandidateSelfApproval) {
		t.Fatalf("self-approval was allowed: %v", err)
	}

	approved, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21),
		f.claim(checker, "approve", c.CandidateID))
	if err != nil || approved.Status != domain.CandidateApproved {
		t.Fatalf("approve: %+v (err=%v)", approved, err)
	}

	inv, err := f.s.IssueInvoice(f.ctx, c.CandidateID, publisher, day(22), f.claim(publisher, "issue", c.CandidateID))
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	if inv.InvoiceNumber != "US-STD-0000000001" {
		t.Fatalf("invoice number = %s", inv.InvoiceNumber)
	}
	if inv.TotalAmount != "52.00" || len(inv.Lines) != 2 {
		t.Fatalf("issued invoice: %+v", inv)
	}
	if n := f.outboxCount(inv.InvoiceID, "platform_invoice.issued"); n != 1 {
		t.Fatalf("platform_invoice.issued events: %d", n)
	}

	// A second candidate for the same already-issued term is refused
	// (negative path #24: correction, never a second invoice for the same
	// population).
	if _, err := f.generateCandidate(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo, "0.00"); !errors.Is(err, domain.ErrInvoiceAlreadyIssuedForTerm) {
		t.Fatalf("a second candidate for an already-issued term was allowed: %v", err)
	}

	// Reading from a different organization sees nothing (RLS tenant
	// isolation, not merely an application-level filter).
	ctxOther := f.tenantCtx(orgB)
	if _, err := f.s.GetInvoice(ctxOther, inv.InvoiceID); !errors.Is(err, domain.ErrInvoiceNotFound) {
		t.Fatalf("cross-org invoice read: %v", err)
	}
	own, err := f.s.GetInvoice(f.ctxOrg, inv.InvoiceID)
	if err != nil || own.InvoiceID != inv.InvoiceID {
		t.Fatalf("own-org invoice read: %+v (err=%v)", own, err)
	}
}

// Negative path #14/§4.4 doctrine carried into COM-05: a METERED component
// with no certified statement for the term is never billed from a live
// counter or silently skipped — it blocks candidate generation outright.
func TestBilling_GenerateInvoiceCandidate_RefusesUncertifiedUsageBasis(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true,
		extra: []domain.PriceComponent{meteredComponent("api.calls", 1, "0", "0.01")}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "10", day(12))
	// Deliberately not closed/certified.

	if _, err := f.generateCandidate(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo, "0.00"); !errors.Is(err, domain.ErrUsageBasisNotCertified) {
		t.Fatalf("generated an invoice candidate from uncertified usage: %v", err)
	}
}

// Negative path #42: a usage statement reopened after the candidate was
// generated (and approved) invalidates issuance — it is never silently
// issued against a stale total.
func TestBilling_IssueInvoice_RefusesWhenUsageBasisChangedSinceGenerate(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true,
		extra: []domain.PriceComponent{meteredComponent("api.calls", 1, "0", "0.01")}})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "10", day(12))
	st := f.closeAndCertify(sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", day(20))

	c, err := f.generateCandidate(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo, "0.00")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	approved, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21), f.claim(checker, "approve", c.CandidateID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Reopen the statement by an independent actor (ReopenWindow's own SoD
	// rule) — this alone must be enough to invalidate the already-approved
	// candidate's usage basis.
	if _, err := f.s.ReopenWindow(f.ctx, st.StatementID, checker, "correction needed", day(22),
		f.claim(checker, "reopen", st.StatementID)); err != nil {
		t.Fatalf("reopen window: %v", err)
	}

	if _, err := f.s.IssueInvoice(f.ctx, approved.CandidateID, publisher, day(23),
		f.claim(publisher, "issue", approved.CandidateID)); !errors.Is(err, domain.ErrInvoiceBasisChanged) {
		t.Fatalf("issued an invoice whose usage basis had changed: %v", err)
	}
}

// Idempotency: replaying GenerateInvoiceCandidate with the same claim key
// never produces a second candidate.
func TestBilling_GenerateInvoiceCandidate_IdempotentReplay(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "10.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	id := domain.NewCommercialID(domain.PrefixInvoiceCandidate)
	req := domain.GenerateInvoiceCandidateRequest{CandidateID: id, OrganizationID: f.org, SubscriptionID: sub.SubscriptionID,
		TermNo: sub.CurrentTerm.TermNo, TaxJurisdictionCode: "US-CA", TaxRateBasisPoints: 875, TaxAmount: "0.88",
		CreatedByPrincipalID: publisher}
	claim := f.claim(publisher, "GenerateInvoiceCandidate", id)
	first, err := f.s.GenerateInvoiceCandidate(f.ctx, req, claim)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	_, err = f.s.GenerateInvoiceCandidate(f.ctx, req, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.CandidateID {
		t.Fatalf("replay of GenerateInvoiceCandidate: %v", err)
	}
}

// DB-level negative control: an issued invoice's row cannot be mutated
// outside the store's own paths, even by the application role.
func TestBilling_IssuedInvoiceIsImmutableAtTheDatabase(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, base: "10.00"})
	f.openBillingAccount(f.org)
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	c, err := f.generateCandidate(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo, "0.00")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := f.s.ApproveInvoiceCandidate(f.ctx, c.CandidateID, checker, day(21), f.claim(checker, "approve", c.CandidateID)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	inv, err := f.s.IssueInvoice(f.ctx, c.CandidateID, publisher, day(22), f.claim(publisher, "issue", c.CandidateID))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		t.Fatalf("declare seller plane: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE platform_commercial_invoices SET total_amount = 0 WHERE invoice_id = $1`, inv.InvoiceID); err == nil {
		t.Fatal("a raw UPDATE against an issued invoice was not rejected")
	}
}

func (f *subFixture) tenantCtx(org string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), org)
}
