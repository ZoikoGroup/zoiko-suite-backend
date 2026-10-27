package domain

import (
	"testing"
)

func sp(s string) *string { return &s }

// evidence builds a clean three-way scenario: one PO line L1 (10 @ 5.00), fully
// received, one invoice line for 10 @ 5.00 = 50.00.
func evidence() MatchEvidence {
	return MatchEvidence{
		Invoice: VendorInvoice{
			InvoiceID: "inv-1", CurrencyCode: "USD", PurchaseOrderID: sp("po-1"), DocumentType: DocInvoice,
			Lines: []VendorInvoiceLine{{InvoiceLineID: "il-1", LineNumber: 1, Quantity: 10, UnitPrice: 5, NetAmount: 50, POLineReference: sp("L1")}},
		},
		PO:            POEvidence{PurchaseOrderID: "po-1", Currency: "USD", Revision: 2, HasRevision: true, HasLines: true, Lines: []POLineEvidence{{LineID: "L1", Quantity: 10, UnitPrice: 5}}},
		Receipts:      ReceiptEvidence{HasLines: true, Received: map[string]float64{"L1": 10}},
		PriorInvoiced: map[string]float64{},
		Policy:        DefaultMatchPolicy("le-1"),
	}
}

func cats(o MatchOutcome) map[string]bool {
	m := map[string]bool{}
	for _, x := range o.Exceptions {
		m[x.Category] = true
	}
	return m
}

func TestMatch_Clean_IsMatched_AndCleared(t *testing.T) {
	o := EvaluateMatch(evidence())
	if o.Result != MatchMatched || !o.Cleared() || len(o.Exceptions) != 0 || o.Lines[0].Result != MatchMatched {
		t.Fatalf("expected a clean MATCHED, got %+v", o)
	}
	if o.Totals.MatchedLines != 1 || o.Totals.InvoiceNet != 50 {
		t.Fatalf("unexpected totals %+v", o.Totals)
	}
}

// Negative path 2: invoice quantity exceeds receipt but auto-matched.
func TestMatch_InvoiceQuantityOverReceipt_IsAnException_NotAutoMatched(t *testing.T) {
	e := evidence()
	e.Receipts.Received["L1"] = 6
	o := EvaluateMatch(e)
	if o.Result != MatchException || o.Cleared() || !cats(o)[ExcQtyOverReceipt] {
		t.Fatalf("expected EXCEPTION with QTY_OVER_RECEIPT, got %+v", o)
	}
	x := o.Exceptions[0]
	if x.Expected != 6 || x.Actual != 10 || x.Difference != 4 || !x.Waivable || x.Class != ClassVariance {
		t.Fatalf("unexpected exception facts %+v", x)
	}
}

// Negative path 4: missing receipt treated as zero-difference match.
func TestMatch_MissingReceipt_IsIncomplete_NeverAMatch(t *testing.T) {
	// A PO line with NO receipt record.
	e := evidence()
	e.Receipts.Received = map[string]float64{}
	o := EvaluateMatch(e)
	if o.Result != MatchIncomplete || o.Cleared() || !cats(o)[ExcMissingReceipt] {
		t.Fatalf("a PO line with no receipt record must be INCOMPLETE/MISSING_RECEIPT, got %+v", o)
	}
	if o.Exceptions[0].Waivable {
		t.Fatal("missing evidence can never be waived")
	}
	// No receipt evidence at all (endpoint down, or old-shape response with no lines).
	for _, r := range []ReceiptEvidence{{Unavailable: true}, {HasLines: false}} {
		e := evidence()
		e.Receipts = r
		o := EvaluateMatch(e)
		if o.Result != MatchIncomplete || !cats(o)[ExcMissingReceiptData] || o.Lines[0].Result != MatchIncomplete {
			t.Fatalf("no receipt evidence must be INCOMPLETE, got %+v", o)
		}
	}
	// A recorded receipt of ZERO is evidence (and the invoice is over it), not an absence.
	e = evidence()
	e.Receipts.Received["L1"] = 0
	if o := EvaluateMatch(e); o.Result != MatchException || !cats(o)[ExcQtyOverReceipt] {
		t.Fatalf("received=0 with an invoiced quantity is an over-receipt exception, got %+v", o)
	}
}

func TestMatch_TwoWay_DoesNotNeedReceipts(t *testing.T) {
	e := evidence()
	e.Policy.Mode = MatchTwoWay
	e.Receipts = ReceiptEvidence{Unavailable: true}
	if o := EvaluateMatch(e); o.Result != MatchMatched {
		t.Fatalf("two-way must ignore receipts, got %+v", o)
	}
	e.PO.Lines[0].Quantity = 4 // ...but the PO is still enforced
	if o := EvaluateMatch(e); o.Result != MatchException || !cats(o)[ExcQtyOverOrder] {
		t.Fatalf("two-way still enforces the ordered quantity, got %+v", o)
	}
}

func TestMatch_MissingPOData_IsIncomplete(t *testing.T) {
	for name, mod := range map[string]func(*MatchEvidence){
		"unavailable":   func(e *MatchEvidence) { e.PO.Unavailable = true },
		"no lines":      func(e *MatchEvidence) { e.PO.HasLines = false },
		"no revision":   func(e *MatchEvidence) { e.PO.HasRevision = false },
		"prior unknown": func(e *MatchEvidence) { e.PriorUnavail = true },
	} {
		e := evidence()
		mod(&e)
		o := EvaluateMatch(e)
		if o.Result != MatchIncomplete || o.Cleared() || !cats(o)[ExcMissingPOData] {
			t.Fatalf("%s: expected INCOMPLETE/MISSING_PO_DATA, got %+v", name, o)
		}
	}
	e := evidence()
	e.Invoice.Lines = nil
	if o := EvaluateMatch(e); o.Result != MatchIncomplete || !cats(o)[ExcMissingInvoiceLines] {
		t.Fatalf("an invoice with no lines cannot match, got %+v", o)
	}
}

func TestMatch_LineMapping(t *testing.T) {
	e := evidence()
	e.Invoice.Lines[0].POLineReference = nil
	if o := EvaluateMatch(e); o.Result != MatchIncomplete || !cats(o)[ExcUnmappedLine] {
		t.Fatalf("an unmapped line is INCOMPLETE, got %+v", o)
	}
	e = evidence()
	e.Invoice.Lines[0].POLineReference = sp("nope")
	if o := EvaluateMatch(e); o.Result != MatchIncomplete || !cats(o)[ExcUnknownPOLine] {
		t.Fatalf("a line naming an unknown PO line is INCOMPLETE, got %+v", o)
	}
}

func TestMatch_CurrencyMismatch_IsNotWaivable(t *testing.T) {
	e := evidence()
	e.Invoice.CurrencyCode = "EUR"
	o := EvaluateMatch(e)
	if o.Result != MatchIncomplete || !cats(o)[ExcCurrencyMismatch] {
		t.Fatalf("expected INCOMPLETE/CURRENCY_MISMATCH, got %+v", o)
	}
	for _, x := range o.Exceptions {
		if x.Category == ExcCurrencyMismatch && x.Waivable {
			t.Fatal("no FX is approved, so a currency mismatch is not waivable")
		}
	}
}

func TestMatch_PriceAndTolerance(t *testing.T) {
	// 1% over: exception at zero tolerance (the default)...
	e := evidence()
	e.Invoice.Lines[0].UnitPrice, e.Invoice.Lines[0].NetAmount = 5.05, 50.5
	o := EvaluateMatch(e)
	if o.Result != MatchException || !cats(o)[ExcPriceVariance] {
		t.Fatalf("expected PRICE_VARIANCE at zero tolerance, got %+v", o)
	}
	if cats(o)[ExcAmountVariance] {
		t.Fatal("a flagged price already explains its own amount; it must not be counted twice")
	}
	// ...WITHIN_TOLERANCE once a policy allows 2%.
	e.Policy = MatchPolicy{LegalEntityID: "le-1", PolicyVersion: 3, Mode: MatchThreeWay, PriceTolerancePct: 2}
	o = EvaluateMatch(e)
	if o.Result != MatchWithinTolerance || !o.Cleared() || len(o.Exceptions) != 0 || o.Lines[0].Result != MatchWithinTolerance {
		t.Fatalf("expected WITHIN_TOLERANCE, got %+v", o)
	}
	// Beyond the tolerance again.
	e.Invoice.Lines[0].UnitPrice, e.Invoice.Lines[0].NetAmount = 5.50, 55
	if o := EvaluateMatch(e); o.Result != MatchException || !cats(o)[ExcPriceVariance] {
		t.Fatalf("10%% over a 2%% tolerance is an exception, got %+v", o)
	}
}

func TestMatch_AmountVariance_WhenPriceMatchesButNetDoesNot(t *testing.T) {
	e := evidence()
	e.Invoice.Lines[0].NetAmount = 58 // an extra 8.00 charge on a correctly priced line
	o := EvaluateMatch(e)
	if o.Result != MatchException || !cats(o)[ExcAmountVariance] || o.Lines[0].AmountVariance != 8 {
		t.Fatalf("expected AMOUNT_VARIANCE of 8.00, got %+v", o)
	}
	e.Policy.AmountToleranceAbs = 10
	if o := EvaluateMatch(e); o.Result != MatchWithinTolerance {
		t.Fatalf("an 8.00 difference within a 10.00 allowance is WITHIN_TOLERANCE, got %+v", o)
	}
}

func TestMatch_CumulativeQuantity_UsesPriorInvoices(t *testing.T) {
	e := evidence()
	e.PriorInvoiced["L1"] = 4 // another invoice already took 4 of the 10
	o := EvaluateMatch(e)     // this one takes 10 more => 14 > 10
	if o.Result != MatchException || !cats(o)[ExcQtyOverOrder] || !cats(o)[ExcQtyOverReceipt] {
		t.Fatalf("over-invoicing across invoices must be caught, got %+v", o)
	}
	// Two lines on the same PO line are judged together, once.
	e = evidence()
	e.Invoice.Lines = []VendorInvoiceLine{
		{InvoiceLineID: "il-1", LineNumber: 1, Quantity: 6, UnitPrice: 5, NetAmount: 30, POLineReference: sp("L1")},
		{InvoiceLineID: "il-2", LineNumber: 2, Quantity: 6, UnitPrice: 5, NetAmount: 30, POLineReference: sp("L1")},
	}
	o = EvaluateMatch(e)
	n := 0
	for _, x := range o.Exceptions {
		if x.Category == ExcQtyOverOrder {
			n++
		}
	}
	if o.Result != MatchException || n != 1 {
		t.Fatalf("12 invoiced against 10 ordered is one over-order exception, got %d in %+v", n, o)
	}
	// Partial invoicing of a PO is fine.
	e = evidence()
	e.Invoice.Lines[0].Quantity, e.Invoice.Lines[0].NetAmount = 4, 20
	if o := EvaluateMatch(e); o.Result != MatchMatched {
		t.Fatalf("invoicing less than ordered/received is fine, got %+v", o)
	}
}

func TestMatch_QuantityTolerance(t *testing.T) {
	e := evidence()
	e.Invoice.Lines[0].Quantity, e.Invoice.Lines[0].NetAmount = 10.4, 52
	e.Receipts.Received["L1"] = 10
	if o := EvaluateMatch(e); o.Result != MatchException {
		t.Fatalf("expected an over-quantity exception at zero tolerance, got %+v", o)
	}
	e.Policy.QtyTolerancePct = 5
	if o := EvaluateMatch(e); o.Result != MatchWithinTolerance {
		t.Fatalf("0.4 over 10 within a 5%% tolerance is WITHIN_TOLERANCE, got %+v", o)
	}
}

// Negative path 1: tolerance widened during an active match. The policy is part of
// the frozen input, so a different tolerance is a different input hash — never the
// same run silently re-read with wider limits.
func TestMatch_ToleranceIsFrozenIntoTheInputHash(t *testing.T) {
	a, b := evidence(), evidence()
	if EvaluateMatch(a).InputHash != EvaluateMatch(b).InputHash {
		t.Fatal("identical evidence must hash identically (deterministic, replayable)")
	}
	b.Policy.PriceTolerancePct = 5
	b.Policy.PolicyVersion = 1
	if EvaluateMatch(a).InputHash == EvaluateMatch(b).InputHash {
		t.Fatal("a widened tolerance must change the input hash")
	}
	c := evidence()
	c.Receipts.Received["L1"] = 9
	if EvaluateMatch(a).InputHash == EvaluateMatch(c).InputHash {
		t.Fatal("changed receipt evidence must change the input hash")
	}
}

// Negative path 3: the engine never self-approves a material variance.
func TestMatch_EngineNeverClearsAVariance(t *testing.T) {
	e := evidence()
	e.Invoice.Lines[0].UnitPrice, e.Invoice.Lines[0].NetAmount = 9, 90
	for i := 0; i < 3; i++ {
		o := EvaluateMatch(e)
		if o.Cleared() || o.Result != MatchException {
			t.Fatalf("an unresolved material variance can never come out cleared, got %+v", o)
		}
		for _, x := range o.Exceptions {
			if x.Status != ExceptionOpen {
				t.Fatalf("the engine only ever raises OPEN exceptions, got %s", x.Status)
			}
		}
	}
}

func TestMatch_IncompleteOutranksVariance(t *testing.T) {
	e := evidence()
	e.Invoice.Lines = append(e.Invoice.Lines, VendorInvoiceLine{InvoiceLineID: "il-2", LineNumber: 2, Quantity: 1, UnitPrice: 1, NetAmount: 1})
	e.Invoice.Lines[0].UnitPrice, e.Invoice.Lines[0].NetAmount = 9, 90
	o := EvaluateMatch(e)
	if o.Result != MatchIncomplete || !cats(o)[ExcPriceVariance] || !cats(o)[ExcUnmappedLine] {
		t.Fatalf("a run that cannot see everything is INCOMPLETE but still reports what it saw, got %+v", o)
	}
}

func TestPolicyValidate_AndDefault(t *testing.T) {
	d := DefaultMatchPolicy("le")
	if d.Mode != MatchThreeWay || d.QtyTolerancePct != 0 || d.PriceTolerancePct != 0 || d.AmountToleranceAbs != 0 || !d.IsDefault || d.PolicyVersion != 0 {
		t.Fatalf("the built-in default must be the strictest reading, got %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]MatchPolicy{
		"bad mode":     {Mode: "FOUR_WAY"},
		"negative":     {Mode: MatchTwoWay, PriceTolerancePct: -1},
		"over 100":     {Mode: MatchTwoWay, QtyTolerancePct: 101},
		"negative abs": {Mode: MatchTwoWay, AmountToleranceAbs: -0.01},
	} {
		if p.Validate() == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestRequiresMatch(t *testing.T) {
	po := sp("po-1")
	cases := []struct {
		name string
		v    VendorInvoice
		want bool
	}{
		{"PO-backed invoice", VendorInvoice{DocumentType: DocInvoice, PurchaseOrderID: po}, true},
		{"legacy row with empty doc type", VendorInvoice{PurchaseOrderID: po}, true},
		{"flag set", VendorInvoice{DocumentType: DocInvoice, MatchRequired: true}, true},
		{"no PO", VendorInvoice{DocumentType: DocInvoice}, false},
		{"empty PO ref", VendorInvoice{DocumentType: DocInvoice, PurchaseOrderID: sp("")}, false},
		{"credit note", VendorInvoice{DocumentType: DocCreditNote, PurchaseOrderID: po}, false},
	}
	for _, c := range cases {
		if got := c.v.RequiresMatch(); got != c.want {
			t.Fatalf("%s: RequiresMatch=%v, want %v", c.name, got, c.want)
		}
	}
}
