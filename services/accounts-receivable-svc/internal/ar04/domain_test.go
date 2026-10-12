package ar04

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

func mustProblem(t *testing.T, err error) *kit.Problem {
	t.Helper()
	var p *kit.Problem
	require.ErrorAs(t, err, &p)
	return p
}

func TestStateMachine(t *testing.T) {
	ok := []struct {
		from State
		tr   Transition
		to   State
	}{
		{StateDraft, TrSubmit, StatePendingApproval}, {StatePendingApproval, TrApprove, StateApproved}, {StateApproved, TrIssue, StateIssued},
		{StateDraft, TrAmend, StateDraft}, {StatePendingApproval, TrAmend, StateDraft}, {StateApproved, TrAmend, StateDraft},
		{StateApproved, TrInvalidate, StateDraft}, {StatePendingApproval, TrInvalidate, StateDraft},
		{StateDraft, TrCancel, StateCancelled}, {StatePendingApproval, TrCancel, StateCancelled}, {StateApproved, TrCancel, StateCancelled},
		{StateIssued, TrCorrect, StateCorrected},
	}
	for _, c := range ok {
		got, err := Next(c.from, c.tr)
		require.NoError(t, err, "%s %s", c.from, c.tr)
		require.Equal(t, c.to, got)
	}
	bad := []struct {
		from State
		tr   Transition
	}{
		{StateDraft, TrApprove}, {StateDraft, TrIssue}, {StatePendingApproval, TrIssue}, {StateApproved, TrSubmit}, {StateDraft, TrCorrect},
		{StateCancelled, TrSubmit}, {StateCancelled, TrIssue}, {StateCancelled, TrAmend}, {StateCancelled, TrCancel}, {StateCorrected, TrCorrect},
	}
	for _, c := range bad {
		_, err := Next(c.from, c.tr)
		p := mustProblem(t, err)
		require.Equal(t, kit.CodeInvalidTransition, p.Code, "%s %s", c.from, c.tr)
		require.Equal(t, http.StatusConflict, p.Status)
	}
	// An issued invoice refuses every content move with the immutability class, and re-issue with INVOICE_ALREADY_ISSUED.
	for _, s := range []State{StateIssued, StateCorrected} {
		for _, tr := range []Transition{TrAmend, TrSubmit, TrApprove, TrCancel, TrInvalidate} {
			_, err := Next(s, tr)
			require.Equal(t, kit.CodeImmutable, mustProblem(t, err).Code, "%s %s", s, tr)
		}
		_, err := Next(s, TrIssue)
		require.Equal(t, kit.CodeInvoiceAlreadyIssued, mustProblem(t, err).Code)
	}
	for _, s := range []State{StateDraft, StatePendingApproval, StateApproved} {
		require.True(t, s.CanAmend(), s)
		require.False(t, s.IsIssued(), s)
	}
	for _, s := range []State{StateIssued, StateCorrected, StateCancelled} {
		require.False(t, s.CanAmend(), s)
	}
	require.True(t, StateIssued.IsIssued() && StateCorrected.IsIssued() && !StateCancelled.IsIssued())
}

func TestDimensionsAreOrthogonalAndValidated(t *testing.T) {
	init := InitialDimensions()
	require.NoError(t, init.Validate(StateDraft))
	require.NoError(t, init.Validate(StateIssued))
	for _, s := range []State{StateDraft, StatePendingApproval, StateApproved, StateCancelled} {
		for name, mut := range map[string]func(*Dimensions){
			"delivery":   func(d *Dimensions) { d.Delivery = DeliverySent },
			"einvoice":   func(d *Dimensions) { d.EInvoice = EInvRequired },
			"settlement": func(d *Dimensions) { d.Settlement = "PAID" },
			"dispute":    func(d *Dimensions) { d.Dispute = "DISPUTED" },
			"attention":  func(d *Dimensions) { d.Attention = AttentionCancelReq },
		} {
			d := init
			mut(&d)
			require.Error(t, d.Validate(s), "%s dimension may not leave its initial value while %s", name, s)
		}
	}
	// Once issued every dimension moves on its own, independent of the others.
	for _, s := range []State{StateIssued, StateCorrected} {
		d := Dimensions{Delivery: DeliveryFailed, EInvoice: "PENDING_UNKNOWN", Settlement: "PARTIALLY_PAID", Dispute: "DISPUTED", Attention: AttentionCancelReq}
		require.NoError(t, d.Validate(s))
	}
	for name, d := range map[string]Dimensions{
		"delivery":   {"BOGUS", "NOT_REQUIRED", "UNPAID", "NONE", "NONE"},
		"einvoice":   {"NOT_SENT", "BOGUS", "UNPAID", "NONE", "NONE"},
		"settlement": {"NOT_SENT", "NOT_REQUIRED", "BOGUS", "NONE", "NONE"},
		"dispute":    {"NOT_SENT", "NOT_REQUIRED", "UNPAID", "BOGUS", "NONE"},
		"attention":  {"NOT_SENT", "NOT_REQUIRED", "UNPAID", "NONE", "BOGUS"},
	} {
		require.Error(t, d.Validate(StateIssued), name)
	}
	for _, s := range []string{"NOT_REQUIRED", "REQUIRED", "PREPARED", "SUBMITTED", "PENDING_UNKNOWN", "ACCEPTED", "REJECTED"} {
		require.True(t, ValidEInvoiceState(s), s)
	}
	for _, s := range []string{"UNPAID", "PARTIALLY_PAID", "PAID", "WRITTEN_OFF", "CREDITED"} {
		require.True(t, ValidSettlementState(s), s)
	}
	require.True(t, ValidDisputeState("NONE") && ValidDisputeState("DISPUTED"))
	require.False(t, ValidEInvoiceState("PAID") || ValidSettlementState("ACCEPTED") || ValidDisputeState("UNPAID"), "the sets never overlap")
}

func TestNextDelivery(t *testing.T) {
	ok := [][2]string{{DeliveryNotSent, DeliverySent}, {DeliveryNotSent, DeliveryFailed}, {DeliverySent, DeliveryDelivered}, {DeliverySent, DeliveryFailed},
		{DeliveryFailed, DeliverySent}, {DeliveryFailed, DeliveryDelivered}, {DeliverySent, DeliverySent}}
	for _, c := range ok {
		require.NoError(t, NextDelivery(c[0], c[1]), "%s -> %s", c[0], c[1])
	}
	for _, c := range [][2]string{{DeliveryDelivered, DeliverySent}, {DeliveryDelivered, DeliveryFailed}, {DeliveryNotSent, DeliveryDelivered}} {
		require.Equal(t, kit.CodeInvalidTransition, mustProblem(t, NextDelivery(c[0], c[1])).Code, "%s -> %s", c[0], c[1])
	}
	require.Equal(t, kit.CodeValidationFailed, mustProblem(t, NextDelivery(DeliverySent, DeliveryNotSent)).Code)
	require.Equal(t, kit.CodeValidationFailed, mustProblem(t, NextDelivery(DeliverySent, "BOGUS")).Code)
}

func TestBuildLinesAreExactAndRoundedOnceToTheCurrency(t *testing.T) {
	lines, err := BuildLines("USD", []LineInput{
		{Description: "A", Quantity: kit.MustAmount("2"), UnitPrice: kit.MustAmount("500")},
		{Description: "B", Quantity: kit.MustAmount("3"), UnitPrice: kit.MustAmount("0.3333")}, // 0.9999 -> 1.00
		{Description: "C", Quantity: kit.MustAmount("0.5"), UnitPrice: kit.MustAmount("0.01")}, // 0.005 -> 0.01 half-up
	})
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("1000"), lines[0].Net)
	require.Equal(t, kit.MustAmount("1.00"), lines[1].Net)
	require.Equal(t, kit.MustAmount("0.01"), lines[2].Net)
	for _, l := range lines {
		require.True(t, l.Net.FitsCurrency("USD"))
	}
	jpy, err := BuildLines("JPY", []LineInput{{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("10.5")}})
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("11"), jpy[0].Net, "JPY has no minor unit: the net is rounded half-up once")
	require.True(t, jpy[0].Net.FitsCurrency("JPY"))
	kwd, err := BuildLines("KWD", []LineInput{{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1.2345")}})
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("1.235"), kwd[0].Net)
}

func TestBuildLinesValidation(t *testing.T) {
	good := LineInput{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")}
	mut := map[string]func(*LineInput){
		"blank description": func(l *LineInput) { l.Description = " " },
		"zero quantity":     func(l *LineInput) { l.Quantity = 0 },
		"negative price":    func(l *LineInput) { l.UnitPrice = kit.MustAmount("-1") },
		"huge quantity":     func(l *LineInput) { l.Quantity = kit.MustAmount("1000000001") },
		"bad product ref":   func(l *LineInput) { l.ProductRef = "has space" },
		"bad tax category":  func(l *LineInput) { l.TaxCategory = "std rate" },
		"bad supply kind":   func(l *LineInput) { l.SupplyKind = "WIDGETS" },
		"bad class":         func(l *LineInput) { l.ProductClassification = "a b" },
	}
	for name, f := range mut {
		l := good
		f(&l)
		_, err := BuildLines("USD", []LineInput{l})
		require.Equal(t, kit.CodeValidationFailed, mustProblem(t, err).Code, name)
	}
	_, err := BuildLines("USD", nil)
	require.Error(t, err)
	many := make([]LineInput, maxLines+1)
	for i := range many {
		many[i] = good
	}
	_, err = BuildLines("USD", many)
	require.Error(t, err)
}

func taxedLines(t *testing.T) ([]Line, []TaxGroup) {
	t.Helper()
	lines, err := BuildLines("USD", []LineInput{
		{Description: "A", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("100.01"), TaxCategory: "STD", ProductClassification: "P1", SupplyKind: "GOODS"},
		{Description: "B", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("200.02"), TaxCategory: "STD", ProductClassification: "P1", SupplyKind: "GOODS"},
		{Description: "C", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("50.00"), TaxCategory: "ZERO", ProductClassification: "P2", SupplyKind: "SERVICES"},
	})
	require.NoError(t, err)
	specs := BuildGroups(lines)
	require.Len(t, specs, 2)
	groups := make([]TaxGroup, len(specs))
	for i, g := range specs {
		tax := mulRound(g.Net, kit.MustAmount("7.5"), 100*10000, "USD")
		if g.TaxCategory == "ZERO" {
			tax = 0
		}
		groups[i] = TaxGroup{GroupNo: g.GroupNo, TaxCategory: g.TaxCategory, ProductClassification: g.ProductClassification, SupplyKind: g.SupplyKind,
			DeterminationID: "det-" + g.TaxCategory, Provenance: "snap-1", Net: g.Net, Taxable: g.Net, Tax: tax}
	}
	return lines, groups
}

func TestTaxIsAllocatedExactlyAndTotalsAddUp(t *testing.T) {
	lines, groups := taxedLines(t)
	require.NoError(t, ApplyTax(lines, groups, "USD"))
	tot, err := TotalsOf(lines, groups, "USD")
	require.NoError(t, err)
	// 300.03 * 7.5% = 22.50225 -> 22.50 ; the two STD lines carry exactly that between them.
	require.Equal(t, kit.MustAmount("350.03"), tot.Net)
	require.Equal(t, kit.MustAmount("22.50"), tot.Tax)
	require.Equal(t, kit.MustAmount("372.53"), tot.Gross)
	require.Equal(t, tot.Net.Add(tot.Tax), tot.Gross)
	var lineTax kit.Amount
	for _, l := range lines {
		lineTax = lineTax.Add(l.Tax)
		require.True(t, l.Tax.FitsCurrency("USD"))
	}
	require.Equal(t, tot.Tax, lineTax, "line taxes add up to the determined tax exactly")
	require.Zero(t, lines[2].Tax)

	// A tampered line breaks the proof.
	lines[0].Net = lines[0].Net.Add(kit.MustAmount("0.01"))
	_, err = TotalsOf(lines, groups, "USD")
	require.Error(t, err)
}

func TestApplyTaxNeverCreatesOrLosesAPenny(t *testing.T) {
	for _, tax := range []string{"0.01", "0.02", "0.03", "7.77", "100.00", "33.33"} {
		lines, err := BuildLines("USD", []LineInput{
			{Description: "A", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("33.33"), TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS"},
			{Description: "B", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("33.33"), TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS"},
			{Description: "C", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("33.34"), TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS"},
		})
		require.NoError(t, err)
		g := BuildGroups(lines)[0]
		groups := []TaxGroup{{GroupNo: 1, TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS", DeterminationID: "d", Provenance: "p", Net: g.Net, Taxable: g.Net, Tax: kit.MustAmount(tax)}}
		require.NoError(t, ApplyTax(lines, groups, "USD"))
		var sum kit.Amount
		for _, l := range lines {
			sum = sum.Add(l.Tax)
		}
		require.Equal(t, kit.MustAmount(tax), sum, tax)
	}
	// JPY: allocation is in whole yen.
	lines, err := BuildLines("JPY", []LineInput{
		{Description: "A", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("100"), TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS"},
		{Description: "B", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("100"), TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS"},
		{Description: "C", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("100"), TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS"},
	})
	require.NoError(t, err)
	g := BuildGroups(lines)[0]
	require.NoError(t, ApplyTax(lines, []TaxGroup{{GroupNo: 1, TaxCategory: "S", ProductClassification: "P", SupplyKind: "GOODS", Net: g.Net, Tax: kit.MustAmount("31")}}, "JPY"))
	var sum kit.Amount
	for _, l := range lines {
		require.True(t, l.Tax.FitsCurrency("JPY"), l.Tax.String())
		sum = sum.Add(l.Tax)
	}
	require.Equal(t, kit.MustAmount("31"), sum)
}

func TestBuildGroupsIsDeterministic(t *testing.T) {
	a, _ := BuildLines("USD", []LineInput{
		{Description: "1", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1"), TaxCategory: "B", ProductClassification: "X", SupplyKind: "GOODS"},
		{Description: "2", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("2"), TaxCategory: "A", ProductClassification: "X", SupplyKind: "GOODS"},
		{Description: "3", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("3"), TaxCategory: "B", ProductClassification: "X", SupplyKind: "GOODS"},
	})
	g := BuildGroups(a)
	require.Len(t, g, 2)
	require.Equal(t, "A", g[0].TaxCategory)
	require.Equal(t, 1, g[0].GroupNo)
	require.Equal(t, "B", g[1].TaxCategory)
	require.Equal(t, kit.MustAmount("4"), g[1].Net)
	require.Equal(t, []int{1, 3}, g[1].LineNos)
	require.Equal(t, g, BuildGroups(a))
}

func TestGroupFromDeterminationRefusesAnythingUnusable(t *testing.T) {
	spec := GroupSpec{GroupNo: 1, TaxCategory: "STD", ProductClassification: "P", SupplyKind: "GOODS", Net: kit.MustAmount("100")}
	good := func() *taxdet.Determination {
		return &taxdet.Determination{DeterminationID: "d1", LegalEntityID: "E", RuleID: "r", TaxLogicSnapshotID: "snap", Status: "CALCULATED",
			Tax: kit.MustAmount("20"), TaxableAmount: kit.MustAmount("100"), Currency: "USD"}
	}
	g, err := GroupFromDetermination(good(), spec, "E", "USD", "u", testNow)
	require.NoError(t, err)
	require.Equal(t, "snap", g.Provenance)
	require.Equal(t, kit.MustAmount("20"), g.Tax)
	bad := map[string]func(*taxdet.Determination){
		"reversed":     func(d *taxdet.Determination) { d.Status = "REVERSED" },
		"other entity": func(d *taxdet.Determination) { d.LegalEntityID = "OTHER" },
		"other ccy":    func(d *taxdet.Determination) { d.Currency = "EUR" },
		"no id":        func(d *taxdet.Determination) { d.DeterminationID = "" },
		"no provenance": func(d *taxdet.Determination) {
			d.TaxLogicSnapshotID, d.RuleID = "", ""
		},
		"negative tax": func(d *taxdet.Determination) { d.Tax = kit.MustAmount("-1") },
		"sub-cent tax": func(d *taxdet.Determination) { d.Tax = kit.MustAmount("1.005") },
	}
	for name, f := range bad {
		d := good()
		f(d)
		_, err := GroupFromDetermination(d, spec, "E", "USD", "u", testNow)
		require.Equal(t, kit.CodeTaxDeterminationMissing, mustProblem(t, err).Code, name)
	}
	_, err = GroupFromDetermination(nil, spec, "E", "USD", "u", testNow)
	require.Equal(t, kit.CodeTaxDeterminationMissing, mustProblem(t, err).Code)
}

func sampleHeader(t *testing.T) (*Header, []Line, []TaxGroup) {
	t.Helper()
	lines, groups := taxedLines(t)
	require.NoError(t, ApplyTax(lines, groups, "USD"))
	tot, err := TotalsOf(lines, groups, "USD")
	require.NoError(t, err)
	h := &Header{InvoiceID: "11111111-1111-1111-1111-111111111111", TenantID: "t", LegalEntityID: "e", CustomerPartyID: "c", Currency: "USD",
		Content:  Content{SupplyDate: "2026-10-10", CustomerRef: "PO", Tax: TaxContext{JurisdictionID: "GB", SupplyType: "B2B"}},
		NetTotal: tot.Net, TaxTotal: tot.Tax, GrossTotal: tot.Gross, ApprovedHash: "h", ApprovedBy: "approver"}
	at := testNow
	h.ApprovedAt = &at
	return h, lines, groups
}

func TestContentHashIsDeterministicAndBindsEverything(t *testing.T) {
	h, lines, groups := sampleHeader(t)
	base := ContentHash(h, lines, groups)
	require.Len(t, base, 64)
	require.Equal(t, base, ContentHash(h, lines, groups), "deterministic")

	changes := map[string]func(*Header, []Line, []TaxGroup){
		"supply date": func(h *Header, _ []Line, _ []TaxGroup) { h.Content.SupplyDate = "2026-10-11" },
		"due date":    func(h *Header, _ []Line, _ []TaxGroup) { h.Content.RequestedDueDate = "2026-12-01" },
		"customer ref": func(h *Header, _ []Line, _ []TaxGroup) {
			h.Content.CustomerRef = "PO2"
		},
		"notes":       func(h *Header, _ []Line, _ []TaxGroup) { h.Content.Notes = "x" },
		"order":       func(h *Header, _ []Line, _ []TaxGroup) { h.Content.OrderID, h.Content.MilestoneID = "o", "m" },
		"tax context": func(h *Header, _ []Line, _ []TaxGroup) { h.Content.Tax.SupplyType = "B2C" },
		"customer":    func(h *Header, _ []Line, _ []TaxGroup) { h.CustomerPartyID = "c2" },
		"entity":      func(h *Header, _ []Line, _ []TaxGroup) { h.LegalEntityID = "e2" },
		"currency":    func(h *Header, _ []Line, _ []TaxGroup) { h.Currency = "EUR" },
		"line desc":   func(_ *Header, l []Line, _ []TaxGroup) { l[0].Description = "other" },
		"line price":  func(_ *Header, l []Line, _ []TaxGroup) { l[0].UnitPrice = l[0].UnitPrice.Add(1) },
		"line net":    func(_ *Header, l []Line, _ []TaxGroup) { l[0].Net = l[0].Net.Add(1) },
		"line tax":    func(_ *Header, l []Line, _ []TaxGroup) { l[0].Tax = l[0].Tax.Add(1) },
		"line class":  func(_ *Header, l []Line, _ []TaxGroup) { l[0].ProductClassification = "Z" },
		"tax amount":  func(_ *Header, _ []Line, g []TaxGroup) { g[0].Tax = g[0].Tax.Add(1) },
		"tax prov":    func(_ *Header, _ []Line, g []TaxGroup) { g[0].Provenance = "snap-2" },
		"tax det id":  func(_ *Header, _ []Line, g []TaxGroup) { g[0].DeterminationID = "other" },
	}
	for name, f := range changes {
		hh, ll, gg := *h, append([]Line(nil), lines...), append([]TaxGroup(nil), groups...)
		f(&hh, ll, gg)
		require.NotEqual(t, base, ContentHash(&hh, ll, gg), name)
	}
	// The legal number, issue date and approval fields are NOT part of the approval-binding hash.
	hh := *h
	hh.InvoiceNumber, hh.IssueDate, hh.ApprovedBy, hh.Version = "INV-1", "2026-10-12", "someone", 9
	require.Equal(t, base, ContentHash(&hh, lines, groups))
	require.NotEqual(t, base, ContentHash(h, lines, nil), "dropping the tax result changes the hash: an approval cannot survive losing its tax")
}

func issueInput(t *testing.T) IssueInput {
	h, lines, groups := sampleHeader(t)
	return IssueInput{H: h, Lines: lines, Groups: groups, Number: "INV-000007", Series: "INV", Sequence: 7, IssueDate: "2026-10-12", DueDate: "2026-11-11",
		TermsCode: "NET30", TermsDays: 30, IssuedBy: "issuer", IssuedAt: testNow, EInvoiceRequired: true}
}

func TestIssuedRepresentationIsCanonicalAndSealed(t *testing.T) {
	in := issueInput(t)
	c1, h1, err := BuildIssued(in)
	require.NoError(t, err)
	c2, h2, err := BuildIssued(in)
	require.NoError(t, err)
	require.Equal(t, c1, c2, "same input, same bytes")
	require.Equal(t, h1, h2)
	require.Equal(t, HashOf(c1), h1, "the hash is the sha256 of the stored text")
	require.Len(t, h1, 64)

	var d IssuedDocument
	require.NoError(t, json.Unmarshal([]byte(c1), &d))
	require.Equal(t, SchemaIssuedInvoice, d.Schema)
	require.Equal(t, "INV-000007", d.InvoiceNumber)
	require.EqualValues(t, 7, d.SequenceNo)
	require.Equal(t, in.H.GrossTotal, d.Gross)
	require.Len(t, d.Lines, 3)
	require.Len(t, d.TaxGroups, 2)
	require.Equal(t, "snap-1", d.TaxGroups[0].Provenance)
	require.True(t, d.EInvoiceRequired)
	require.Equal(t, in.H.NetTotal.Add(in.H.TaxTotal), d.Gross)

	in.Number = "INV-000008"
	_, h3, err := BuildIssued(in)
	require.NoError(t, err)
	require.NotEqual(t, h1, h3, "the number is sealed into the hash")
	in = issueInput(t)
	in.Lines[0].Tax = in.Lines[0].Tax.Add(1)
	_, h4, _ := BuildIssued(in)
	require.NotEqual(t, h1, h4)
	// Tampered text no longer hashes to the seal.
	require.NotEqual(t, h1, HashOf(c1+" "))
}

func TestNumberingFormat(t *testing.T) {
	require.Equal(t, "INV-000001", DefaultNumbering.Format(1))
	require.Equal(t, "INV-123456", DefaultNumbering.Format(123456))
	require.Equal(t, "INV-1234567", DefaultNumbering.Format(1234567), "a number wider than the width is never truncated")
	p := NumberingProfile{Series: "FA", Prefix: "FA/2026/", Width: 4}
	require.Equal(t, "FA/2026/0042", p.Format(42))
	require.Equal(t, "7", NumberingProfile{Series: "X", Width: 1}.Format(7))
	require.NoError(t, p.Validate())
	require.Error(t, NumberingProfile{Series: "bad series", Width: 4}.Validate())
	require.Error(t, NumberingProfile{Series: "OK", Width: 0}.Validate())
	require.Error(t, NumberingProfile{Series: "OK", Width: 13}.Validate())
}

func TestMateriality(t *testing.T) {
	require.False(t, IsMaterial(MaterialInvoiceThreshold), "the threshold itself is not material")
	require.True(t, IsMaterial(MaterialInvoiceThreshold.Add(kit.MustAmount("0.01"))))
	require.False(t, IsMaterial(0))
}

func TestContentValidate(t *testing.T) {
	ok := Content{SupplyDate: "2026-10-10", Tax: TaxContext{JurisdictionID: "GB", SupplyType: "B2B"}}
	require.NoError(t, ok.Validate())
	bad := map[string]func(*Content){
		"no supply date":     func(c *Content) { c.SupplyDate = "" },
		"bad supply date":    func(c *Content) { c.SupplyDate = "10/10/2026" },
		"bad due date":       func(c *Content) { c.RequestedDueDate = "soon" },
		"order w/o mile":     func(c *Content) { c.OrderID = "6f1b8a52-3d1c-4b4e-8b6f-0d9f0a6b2c11" },
		"mile w/o order":     func(c *Content) { c.MilestoneID = "6f1b8a52-3d1c-4b4e-8b6f-0d9f0a6b2c11" },
		"bad order id":       func(c *Content) { c.OrderID, c.MilestoneID = "x", "6f1b8a52-3d1c-4b4e-8b6f-0d9f0a6b2c11" },
		"bad supply type":    func(c *Content) { c.Tax.SupplyType = "B2X" },
		"bad jurisdiction":   func(c *Content) { c.Tax.JurisdictionID = "a b" },
		"long notes":         func(c *Content) { c.Notes = string(make([]byte, maxText+1)) },
		"long customer_ref ": func(c *Content) { c.CustomerRef = string(make([]rune, 256)) },
	}
	for name, f := range bad {
		c := ok
		f(&c)
		require.Error(t, c.Validate(), name)
	}
	lines, _ := BuildLines("USD", []LineInput{{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")}})
	require.Error(t, ValidateForTax(ok, lines), "tax inputs per line are required before submit")
	lines[0].TaxCategory, lines[0].ProductClassification, lines[0].SupplyKind = "STD", "P", "GOODS"
	require.NoError(t, ValidateForTax(ok, lines))
	nj := ok
	nj.Tax.JurisdictionID = ""
	require.Error(t, ValidateForTax(nj, lines))
	nt := ok
	nt.Tax.SupplyType = ""
	require.Error(t, ValidateForTax(nt, lines))
}

func TestToIssuedMapsTheKitView(t *testing.T) {
	h, lines, groups := sampleHeader(t)
	h.State, h.InvoiceNumber, h.IssueDate, h.DueDate, h.IssuedContentHash = StateIssued, "INV-000001", "2026-10-12", "2026-11-11", "abc"
	v := toIssued(&Invoice{Header: *h, Lines: lines, TaxGroups: groups})
	require.Equal(t, "INV-000001", v.InvoiceNumber)
	require.Equal(t, h.GrossTotal, v.Gross)
	require.Equal(t, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), v.IssueDate)
	require.Equal(t, "det-STD,det-ZERO", v.TaxDeterminationID)
	require.Equal(t, "snap-1", v.TaxRulePackVersion)
	require.Equal(t, "abc", v.ContentHash)
	require.Len(t, v.Lines, 3)
}
