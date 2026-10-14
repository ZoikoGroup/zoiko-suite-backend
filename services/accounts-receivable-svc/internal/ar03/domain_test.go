package ar03

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func TestStateMachine(t *testing.T) {
	ok := []struct {
		from State
		tr   Transition
		to   State
	}{
		{StateDraft, TrSubmit, StatePendingApproval}, {StatePendingApproval, TrApprove, StateApproved}, {StateApproved, TrConfirm, StateConfirmed},
		{StateConfirmed, TrFulfil, StateInFulfillment}, {StateConfirmed, TrPartBill, StatePartiallyBilled}, {StateInFulfillment, TrPartBill, StatePartiallyBilled},
		{StatePartiallyBilled, TrFullBill, StateFullyBilled}, {StateInFulfillment, TrFullBill, StateFullyBilled}, {StateFullyBilled, TrClose, StateClosed},
		{StatePartiallyBilled, TrClose, StateClosed}, {StateConfirmed, TrClose, StateClosed},
		{StateDraft, TrCancel, StateCancelled}, {StateApproved, TrCancel, StateCancelled}, {StatePartiallyBilled, TrCancel, StateCancelled},
		{StateConfirmed, TrPlaceHold, StateConfirmed}, {StatePendingApproval, TrPlaceHold, StatePendingApproval},
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
		{StateDraft, TrApprove}, {StateDraft, TrConfirm}, {StatePendingApproval, TrConfirm}, {StateApproved, TrSubmit}, {StateDraft, TrFulfil},
		{StateDraft, TrClose}, {StatePendingApproval, TrClose}, {StateFullyBilled, TrCancel}, {StateClosed, TrCancel}, {StateCancelled, TrCancel},
		{StateCancelled, TrConfirm}, {StateCancelled, TrClose}, {StateClosed, TrClose}, {StateFullyBilled, TrFullBill}, {StateDraft, TrPlaceHold},
		{StateCancelled, TrPlaceHold}, {StateFullyBilled, TrPlaceHold}, {StatePartiallyBilled, TrConfirm}, {StateConfirmed, TrApprove},
	}
	for _, c := range bad {
		_, err := Next(c.from, c.tr)
		var p *kit.Problem
		require.ErrorAs(t, err, &p, "%s %s", c.from, c.tr)
		require.Equal(t, kit.CodeInvalidTransition, p.Code)
		require.Equal(t, http.StatusConflict, p.Status)
	}
}

func TestAmendAndBillableStateRules(t *testing.T) {
	for _, s := range []State{StateDraft, StatePendingApproval, StateApproved} {
		require.True(t, s.CanAmendDraft(), s)
		require.False(t, s.CanAmendLive(), s)
		require.False(t, s.Billable(), s)
	}
	for _, s := range []State{StateConfirmed, StateInFulfillment, StatePartiallyBilled} {
		require.True(t, s.CanAmendLive(), s)
		require.False(t, s.CanAmendDraft(), s)
		require.True(t, s.Billable(), s)
	}
	for _, s := range []State{StateFullyBilled, StateClosed, StateCancelled} {
		require.False(t, s.CanAmendLive() || s.CanAmendDraft() || s.Billable(), s)
	}
}

func TestBuildLinesAndDiscountAllocationAreExact(t *testing.T) {
	lines, err := BuildLines("USD", []LineInput{
		{Description: "A", Quantity: kit.MustAmount("2"), UnitPrice: kit.MustAmount("500")},
		{Description: "B", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("250.50")},
	})
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("1000"), lines[0].ListNet)
	tot, err := ApplyDiscountPercent("USD", lines, kit.MustAmount("5"))
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("1250.50"), tot.Subtotal)
	require.Equal(t, kit.MustAmount("62.53"), tot.DiscountAmount, "62.525 rounds half-up once")
	require.Equal(t, kit.MustAmount("1187.97"), tot.NetTotal)
	require.Equal(t, tot, TotalsOf(lines), "the allocated parts add up to the totals")
	for _, l := range lines {
		require.Equal(t, l.ListNet.Sub(l.DiscountAmount), l.LineNet)
		require.True(t, l.DiscountAmount.FitsCurrency("USD"))
	}

	// Awkward split: 3 equal lines, 1.00 discount -> 0.34 / 0.33 / 0.33 (largest remainder, ties to the lower index).
	three, err := BuildLines("USD", []LineInput{
		{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("10")},
		{Description: "y", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("10")},
		{Description: "z", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("10")},
	})
	require.NoError(t, err)
	_, err = ApplyDiscountAmount("USD", three, kit.MustAmount("1"))
	require.NoError(t, err)
	require.Equal(t, []kit.Amount{kit.MustAmount("0.34"), kit.MustAmount("0.33"), kit.MustAmount("0.33")},
		[]kit.Amount{three[0].DiscountAmount, three[1].DiscountAmount, three[2].DiscountAmount})

	// JPY has no minor unit: nets and discounts are whole yen, still summing exactly.
	jpy, err := BuildLines("JPY", []LineInput{
		{Description: "x", Quantity: kit.MustAmount("3"), UnitPrice: kit.MustAmount("333")},
		{Description: "y", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1001")},
	})
	require.NoError(t, err)
	jt, err := ApplyDiscountPercent("JPY", jpy, kit.MustAmount("7.5"))
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("2000"), jt.Subtotal)
	require.Equal(t, kit.MustAmount("150"), jt.DiscountAmount)
	require.Equal(t, kit.MustAmount("1850"), jt.NetTotal)
	for _, l := range jpy {
		require.True(t, l.LineNet.FitsCurrency("JPY"), l.LineNet.String())
		require.True(t, l.DiscountAmount.FitsCurrency("JPY"))
	}
	require.Equal(t, jt.NetTotal, TotalsOf(jpy).NetTotal)

	// KWD has three decimals: 0.001 is a valid unit and 0.0001 is not.
	kwd, err := BuildLines("KWD", []LineInput{{Description: "x", Quantity: kit.MustAmount("3"), UnitPrice: kit.MustAmount("1.333")}})
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("3.999"), kwd[0].ListNet)
	kt, err := ApplyDiscountAmount("KWD", kwd, kit.MustAmount("0.001"))
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("3.998"), kt.NetTotal)
	_, err = ApplyDiscountAmount("KWD", kwd, kit.MustAmount("0.0001"))
	require.Error(t, err, "a discount finer than the currency's minor unit is refused")
	require.NoError(t, Content{Currency: "KWD", Lines: kwd}.Validate())
	require.Error(t, Content{Currency: "JPY", Lines: []ContentLine{{LineNo: 1, ListNet: kit.MustAmount("10.5"), LineNet: kit.MustAmount("10.5")}}}.Validate())
}

func TestBuildLinesValidation(t *testing.T) {
	for name, in := range map[string][]LineInput{
		"empty":         {},
		"blank desc":    {{Description: " ", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")}},
		"zero qty":      {{Description: "x", Quantity: 0, UnitPrice: kit.MustAmount("1")}},
		"negative":      {{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("-1")}},
		"huge":          {{Description: "x", Quantity: kit.MustAmount("2000000000"), UnitPrice: kit.MustAmount("1")}},
		"bad ref":       {{Description: "x", ProductRef: "a b", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")}},
		"net too large": {{Description: "x", Quantity: kit.MustAmount("1000000000"), UnitPrice: kit.MustAmount("1000000000")}},
	} {
		_, err := BuildLines("USD", in)
		var p *kit.Problem
		require.ErrorAs(t, err, &p, name)
		require.Equal(t, kit.CodeValidationFailed, p.Code, name)
	}
}

func TestRemainingToBillMath(t *testing.T) {
	require.Equal(t, kit.MustAmount("4"), Remaining(kit.MustAmount("10"), kit.MustAmount("6")))
	require.Zero(t, Remaining(kit.MustAmount("10"), kit.MustAmount("10")))
	require.Zero(t, Remaining(kit.MustAmount("10"), kit.MustAmount("11")), "never negative")

	mk := func(qty, net, iq, ia string) Line {
		return Line{ContentLine: ContentLine{Quantity: kit.MustAmount(qty), LineNet: kit.MustAmount(net)},
			InvoicedQty: kit.MustAmount(iq), InvoicedAmount: kit.MustAmount(ia),
			RemainingQty: kit.MustAmount(qty).Sub(kit.MustAmount(iq)), RemainingAmount: kit.MustAmount(net).Sub(kit.MustAmount(ia))}
	}
	s := Summarize([]Line{mk("2", "1000", "0", "0"), mk("1", "250.50", "0", "0")})
	require.False(t, s.AnyBilled)
	require.False(t, s.FullyBilled)
	require.Equal(t, kit.MustAmount("1250.50"), s.RemainingNet)
	require.Equal(t, StateConfirmed, StateAfterBilling(StateConfirmed, s))

	s = Summarize([]Line{mk("2", "1000", "1", "500"), mk("1", "250.50", "0", "0")})
	require.True(t, s.AnyBilled)
	require.False(t, s.FullyBilled)
	require.Equal(t, kit.MustAmount("500"), s.InvoicedNet)
	require.Equal(t, kit.MustAmount("750.50"), s.RemainingNet)
	require.Equal(t, StatePartiallyBilled, StateAfterBilling(StateConfirmed, s))
	require.Equal(t, StatePartiallyBilled, StateAfterBilling(StatePartiallyBilled, s))

	s = Summarize([]Line{mk("2", "1000", "2", "1000"), mk("1", "250.50", "1", "250.50")})
	require.True(t, s.FullyBilled)
	require.Equal(t, StateFullyBilled, StateAfterBilling(StatePartiallyBilled, s))
	require.Zero(t, s.RemainingNet)

	// Amount exhausted but quantity left (or vice versa) is not fully billed.
	require.False(t, Summarize([]Line{mk("2", "1000", "1", "1000")}).FullyBilled)
	require.False(t, Summarize([]Line{mk("2", "1000", "2", "999.99")}).FullyBilled)
	require.False(t, Summarize(nil).FullyBilled, "an order without lines is never fully billed")

	// JPY / KWD exactness: no float drift in the sums.
	jp := Summarize([]Line{mk("3", "999", "3", "999"), mk("1", "1001", "0", "0")})
	require.Equal(t, kit.MustAmount("1001"), jp.RemainingNet)
	kw := Summarize([]Line{mk("3", "3.999", "1", "1.333"), mk("1", "0.001", "0", "0")})
	require.Equal(t, kit.MustAmount("2.667"), kw.RemainingNet)
}

func TestCommercialAndCreditThresholds(t *testing.T) {
	noProfile := (*kit.ProfileView)(nil)
	require.False(t, SoDRequired(CommercialApprovalThreshold, "USD", noProfile, false), "exactly the threshold is within the creator's authority")
	require.True(t, SoDRequired(CommercialApprovalThreshold+1, "USD", noProfile, false))
	require.True(t, SoDRequired(kit.MustAmount("1"), "USD", noProfile, true), "an attached credit exception always needs an independent approver")

	p := &kit.ProfileView{CreditLimit: kit.MustAmount("1000"), CreditCurrency: "USD"}
	require.False(t, SoDRequired(kit.MustAmount("500"), "USD", p, false), "exactly 50% of the limit")
	require.True(t, SoDRequired(kit.MustAmount("500.01"), "USD", p, false))
	require.False(t, SoDRequired(kit.MustAmount("900"), "EUR", p, false), "a different currency is not compared")
	require.False(t, SoDRequired(kit.MustAmount("900"), "USD", &kit.ProfileView{}, false), "no limit configured")
}

func TestEvaluateCredit(t *testing.T) {
	p := &kit.ProfileView{CreditLimit: kit.MustAmount("1000"), CreditCurrency: "USD"}
	r := EvaluateCredit(p, "USD", kit.MustAmount("1000"), 0, false)
	require.False(t, r.Exceeded, "equal to the limit is not above it")
	require.False(t, r.Blocked())
	require.False(t, r.ExposureAvailable)

	r = EvaluateCredit(p, "USD", kit.MustAmount("1000.01"), kit.MustAmount("999"), false)
	require.True(t, r.Exceeded, "unavailable exposure is ignored, never assumed: only the order value counts")
	require.Zero(t, r.Exposure)
	require.Contains(t, r.Detail("USD"), "existing exposure is unavailable")

	r = EvaluateCredit(p, "USD", kit.MustAmount("300"), kit.MustAmount("700.01"), true)
	require.True(t, r.Exceeded)
	require.Equal(t, kit.MustAmount("1000.01"), r.Projected)
	require.Contains(t, r.Detail("USD"), "exposure 700.0100 plus this order 300.0000")
	require.False(t, EvaluateCredit(p, "USD", kit.MustAmount("300"), kit.MustAmount("700"), true).Exceeded)

	require.False(t, EvaluateCredit(&kit.ProfileView{}, "USD", kit.MustAmount("9999999"), kit.MustAmount("1"), true).Blocked(), "no limit configured")
	hold := EvaluateCredit(&kit.ProfileView{CreditHold: true}, "USD", 1, 0, false)
	require.True(t, hold.Blocked())
	require.Contains(t, hold.Detail("USD"), "credit hold")
	cur := EvaluateCredit(p, "EUR", 1, 0, false)
	require.True(t, cur.Unverifiable)
	require.True(t, cur.Blocked(), "a limit in another currency cannot be verified, so it fails closed")
}

func TestCheckBillable(t *testing.T) {
	ms := &Milestone{State: MsRecorded}
	billable := &Header{State: StateConfirmed}
	require.Nil(t, CheckBillable(billable, ms))
	cases := map[string]struct {
		h  *Header
		ms *Milestone
	}{
		"cancelled":        {&Header{State: StateCancelled}, ms},
		"closed":           {&Header{State: StateClosed}, ms},
		"fully billed":     {&Header{State: StateFullyBilled}, ms},
		"held":             {&Header{State: StateConfirmed, HoldActive: true, HoldKind: HoldCommercial}, ms},
		"credit held":      {&Header{State: StateInFulfillment, HoldActive: true, HoldKind: HoldCredit}, ms},
		"draft":            {&Header{State: StateDraft}, ms},
		"pending approval": {&Header{State: StatePendingApproval}, ms},
		"approved":         {&Header{State: StateApproved}, ms},
		"unknown ms":       {billable, nil},
		"already billed":   {billable, &Milestone{State: MsInvoiced}},
		"voided":           {billable, &Milestone{State: MsVoided}},
	}
	for name, c := range cases {
		p := CheckBillable(c.h, c.ms)
		require.NotNil(t, p, name)
		require.Equal(t, kit.CodeOrderNotBillable, p.Code, name)
		require.Equal(t, http.StatusUnprocessableEntity, p.Status, name)
	}
	require.Contains(t, CheckBillable(&Header{State: StateCancelled}, ms).Msg, "cancelled")
	require.Contains(t, CheckBillable(&Header{State: StateConfirmed, HoldActive: true, HoldKind: HoldCredit}, ms).Msg, "credit hold")
}

func TestCheckLiveAmendment(t *testing.T) {
	cur := []Line{
		{ContentLine: ContentLine{LineNo: 1, Quantity: kit.MustAmount("10"), LineNet: kit.MustAmount("1000")}, InvoicedQty: kit.MustAmount("4"), InvoicedAmount: kit.MustAmount("400")},
		{ContentLine: ContentLine{LineNo: 2, Quantity: kit.MustAmount("1"), LineNet: kit.MustAmount("250")}},
	}
	next := func(q1, n1, q2, n2 string) []ContentLine {
		return []ContentLine{{LineNo: 1, Quantity: kit.MustAmount(q1), LineNet: kit.MustAmount(n1)}, {LineNo: 2, Quantity: kit.MustAmount(q2), LineNet: kit.MustAmount(n2)}}
	}
	codeOf := func(err error) string {
		var p *kit.Problem
		require.ErrorAs(t, err, &p)
		return p.Code
	}
	require.NoError(t, CheckLiveAmendment(cur, nil, next("10", "1000", "1", "250")))
	require.NoError(t, CheckLiveAmendment(cur, nil, next("4", "400", "1", "100")), "down to exactly what is invoiced is allowed")
	require.Equal(t, kit.CodeImmutable, codeOf(CheckLiveAmendment(cur, nil, next("3", "400", "1", "250"))), "quantity below invoiced")
	require.Equal(t, kit.CodeImmutable, codeOf(CheckLiveAmendment(cur, nil, next("10", "399.99", "1", "250"))), "net below invoiced")
	require.Equal(t, CodeAmendmentNotAllowed, codeOf(CheckLiveAmendment(cur, nil, next("11", "1000", "1", "250"))), "quantity increase")
	require.Equal(t, CodeAmendmentNotAllowed, codeOf(CheckLiveAmendment(cur, nil, next("10", "1000", "1", "250.01"))), "net increase")
	require.Equal(t, CodeAmendmentNotAllowed, codeOf(CheckLiveAmendment(cur, nil, next("10", "1000", "1", "250")[:1])), "a line cannot be dropped")
	open := map[int]OpenMilestoneHold{1: {Qty: kit.MustAmount("2"), Amount: kit.MustAmount("200")}}
	require.NoError(t, CheckLiveAmendment(cur, open, next("6", "600", "1", "250")))
	require.Equal(t, CodeAmendmentNotAllowed, codeOf(CheckLiveAmendment(cur, open, next("5", "600", "1", "250"))), "below what a recorded milestone claims")
	renum := next("10", "1000", "1", "250")
	renum[1].LineNo = 9
	require.Equal(t, CodeAmendmentNotAllowed, codeOf(CheckLiveAmendment(cur, nil, renum)))
}

func TestContentHashBindsEverything(t *testing.T) {
	base := func() Content {
		l, _ := BuildLines("USD", []LineInput{{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("10")}})
		return Content{Currency: "USD", CustomerPORef: "PO-1", Terms: "t", PriceSourceRef: "P", Lines: l}
	}
	h := ContentHash(base())
	require.Len(t, h, 64)
	require.Equal(t, h, ContentHash(base()))
	for name, f := range map[string]func(*Content){
		"po":    func(c *Content) { c.CustomerPORef = "PO-2" },
		"terms": func(c *Content) { c.Terms = "u" },
		"price": func(c *Content) { c.PriceSourceRef = "Q" },
		"disc":  func(c *Content) { c.DiscountPercent = kit.MustAmount("1") },
		"qty":   func(c *Content) { c.Lines[0].Quantity = kit.MustAmount("2") },
		"net":   func(c *Content) { c.Lines[0].LineNet = kit.MustAmount("9") },
	} {
		c := base()
		f(&c)
		require.NotEqual(t, h, ContentHash(c), name)
	}
}
