package ar02

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func TestStateMachine(t *testing.T) {
	ok := []struct {
		from State
		tr   Transition
		to   State
	}{
		{StateDraft, TrSubmit, StatePendingApproval}, {StatePendingApproval, TrApprove, StateApproved}, {StateApproved, TrSend, StateSent},
		{StateSent, TrAccept, StateAccepted}, {StateAccepted, TrConvert, StateConverted},
		{StatePendingApproval, TrReject, StateRejected}, {StateApproved, TrReject, StateRejected}, {StateSent, TrReject, StateRejected},
		{StateDraft, TrExpire, StateExpired}, {StateSent, TrExpire, StateExpired}, {StateAccepted, TrExpire, StateExpired},
	}
	for _, c := range ok {
		got, err := Next(c.from, c.tr)
		require.NoError(t, err)
		require.Equal(t, c.to, got)
	}
	bad := []struct {
		from State
		tr   Transition
	}{
		{StateDraft, TrApprove}, {StateDraft, TrSend}, {StateDraft, TrAccept}, {StateDraft, TrConvert}, {StatePendingApproval, TrSend},
		{StateApproved, TrAccept}, {StateApproved, TrConvert}, {StateSent, TrConvert}, {StateSent, TrApprove},
		{StateConverted, TrConvert}, {StateConverted, TrExpire}, {StateRejected, TrSend}, {StateExpired, TrSend}, {StateExpired, TrConvert},
		{StateRejected, TrExpire}, {StateAccepted, TrReject}, {StateSent, TrSubmit},
	}
	for _, c := range bad {
		_, err := Next(c.from, c.tr)
		var p *kit.Problem
		require.ErrorAs(t, err, &p)
		require.Equal(t, kit.CodeInvalidTransition, p.Code)
		require.Equal(t, http.StatusConflict, p.Status)
	}
}

func TestAmendRules(t *testing.T) {
	for _, s := range []State{StateDraft, StatePendingApproval, StateApproved, StateSent} {
		require.True(t, s.CanAmend(), s)
	}
	for _, s := range []State{StateAccepted, StateRejected, StateExpired, StateConverted} {
		require.False(t, s.CanAmend(), s)
	}
	for _, s := range []State{StateDraft, StatePendingApproval, StateApproved, StateSent} {
		require.Equal(t, StateDraft, AfterAmend(s, true), "a material change always returns the quote to DRAFT")
		require.Equal(t, s, AfterAmend(s, false))
	}
}

func lines(t *testing.T, cur string, in ...LineInput) []Line {
	t.Helper()
	ls, err := BuildLines(cur, in)
	require.NoError(t, err)
	return ls
}

func baseContent(t *testing.T) Content {
	return Content{Currency: "USD", ValidityDate: "2026-12-31", Terms: "Net 30", Assumptions: "a", DiscountPercent: kit.MustAmount("5"),
		PriceSourceRef: "PL-1", CatalogVersionRef: "CAT-1",
		Lines: lines(t, "USD", LineInput{Description: "Widget", Quantity: kit.MustAmount("3"), UnitPrice: kit.MustAmount("10.00")})}
}

func TestExactLineAndTotalArithmetic(t *testing.T) {
	// 3 x 33.3333 = 99.9999 -> 100.00 half-up to the cent; never float.
	require.Equal(t, kit.MustAmount("100.00"), LineNet(kit.MustAmount("3"), kit.MustAmount("33.3333"), "USD"))
	require.Equal(t, kit.MustAmount("0.01"), LineNet(kit.MustAmount("1"), kit.MustAmount("0.005"), "USD"), "ties round up")
	require.Equal(t, kit.MustAmount("0.00"), LineNet(kit.MustAmount("1"), kit.MustAmount("0.0049"), "USD"))
	require.Equal(t, kit.MustAmount("13"), LineNet(kit.MustAmount("1.5"), kit.MustAmount("8.5"), "JPY"), "12.75 JPY -> whole yen")
	require.Equal(t, kit.MustAmount("1.235"), LineNet(kit.MustAmount("1"), kit.MustAmount("1.2345"), "KWD"), "3-digit currency")
	// Large values do not overflow (big-integer product); an out-of-range net is refused.
	require.Equal(t, kit.MustAmount("999999000000.00"), LineNet(kit.MustAmount("999999"), kit.MustAmount("1000000"), "USD"))
	_, err := BuildLines("USD", []LineInput{{Description: "x", Quantity: kit.MustAmount("1000000000"), UnitPrice: kit.MustAmount("1000000000")}})
	require.Error(t, err)

	ls := lines(t, "USD",
		LineInput{Description: "a", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("19.99")},
		LineInput{Description: "b", Quantity: kit.MustAmount("2.5"), UnitPrice: kit.MustAmount("7.77")})
	tot, err := ComputeTotals("USD", ls, kit.MustAmount("12.5"))
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("39.42"), tot.Subtotal) // 19.99 + 19.425 -> 19.43
	require.Equal(t, kit.MustAmount("4.93"), tot.DiscountAmount)
	require.Equal(t, tot.Subtotal.Sub(tot.DiscountAmount), tot.NetTotal)
	for _, a := range []kit.Amount{tot.Subtotal, tot.DiscountAmount, tot.NetTotal} {
		require.True(t, a.FitsCurrency("USD"))
	}
}

func TestBuildLinesValidation(t *testing.T) {
	good := LineInput{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")}
	bad := map[string][]LineInput{
		"none":           nil,
		"blank desc":     {{Description: " ", Quantity: kit.MustAmount("1")}},
		"zero qty":       {{Description: "x", Quantity: 0, UnitPrice: kit.MustAmount("1")}},
		"negative qty":   {{Description: "x", Quantity: kit.MustAmount("-1"), UnitPrice: kit.MustAmount("1")}},
		"negative price": {{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("-1")}},
		"bad product":    {{Description: "x", ProductRef: "has space", Quantity: kit.MustAmount("1")}},
		"huge price":     {{Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1000000001")}},
	}
	for name, in := range bad {
		_, err := BuildLines("USD", in)
		require.Error(t, err, name)
	}
	_, err := BuildLines("USD", []LineInput{good})
	require.NoError(t, err)
	many := make([]LineInput, maxLines+1)
	for i := range many {
		many[i] = good
	}
	_, err = BuildLines("USD", many)
	require.Error(t, err)
}

func TestContentHashAndMaterialChange(t *testing.T) {
	base := baseContent(t)
	require.Equal(t, ContentHash(base), ContentHash(baseContent(t)), "hash is deterministic")
	require.Len(t, ContentHash(base), 64)

	mutate := func(f func(*Content)) Content {
		c := baseContent(t)
		f(&c)
		return c
	}
	material := map[string]Content{
		"quantity": mutate(func(c *Content) {
			c.Lines = lines(t, "USD", LineInput{Description: "Widget", Quantity: kit.MustAmount("4"), UnitPrice: kit.MustAmount("10.00")})
		}),
		"unit price": mutate(func(c *Content) {
			c.Lines = lines(t, "USD", LineInput{Description: "Widget", Quantity: kit.MustAmount("3"), UnitPrice: kit.MustAmount("10.01")})
		}),
		"extra line": mutate(func(c *Content) {
			c.Lines = append(c.Lines, Line{LineNo: 2, Description: "More", Quantity: kit.MustAmount("1"), UnitPrice: 1, LineNet: 1})
		}),
		"discount":        mutate(func(c *Content) { c.DiscountPercent = kit.MustAmount("6") }),
		"currency":        mutate(func(c *Content) { c.Currency = "EUR" }),
		"terms":           mutate(func(c *Content) { c.Terms = "Net 60" }),
		"price source":    mutate(func(c *Content) { c.PriceSourceRef = "PL-2" }),
		"catalog":         mutate(func(c *Content) { c.CatalogVersionRef = "CAT-2" }),
		"validity extend": mutate(func(c *Content) { c.ValidityDate = "2027-01-31" }),
		"tax request":     mutate(func(c *Content) { c.RequestTaxEstimate = true; c.TaxParams = &TaxParams{JurisdictionID: "GB"} }),
	}
	for name, c := range material {
		require.True(t, Material(base, c), name)
		if name != "tax request" { // the tax-estimate request is material but not part of the price/content hash
			require.NotEqual(t, ContentHash(base), ContentHash(c), name)
		}
	}
	nonMaterial := map[string]Content{
		"assumptions":       mutate(func(c *Content) { c.Assumptions = "changed assumption" }),
		"validity shortens": mutate(func(c *Content) { c.ValidityDate = "2026-11-30" }),
	}
	for name, c := range nonMaterial {
		require.False(t, Material(base, c), name)
	}
	// Assumptions are outside the approval-binding hash; validity is inside it.
	require.Equal(t, ContentHash(base), ContentHash(nonMaterial["assumptions"]))
	require.NotEqual(t, ContentHash(base), ContentHash(nonMaterial["validity shortens"]))
	require.True(t, SameContent(base, baseContent(t)))
	require.False(t, SameContent(base, nonMaterial["assumptions"]))
}

func TestDiscountThreshold(t *testing.T) {
	require.False(t, NeedsDiscountAuthority(kit.MustAmount("0")))
	require.False(t, NeedsDiscountAuthority(kit.MustAmount("10")), "exactly the threshold is within the preparer's own authority")
	require.True(t, NeedsDiscountAuthority(kit.MustAmount("10.0001")))
	require.True(t, NeedsDiscountAuthority(kit.MustAmount("100")))
}

func TestExpiry(t *testing.T) {
	today := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	require.False(t, Expired("2026-10-12", today), "valid through the validity date inclusive")
	require.False(t, Expired("2026-10-13", today))
	require.True(t, Expired("2026-10-11", today))
}

func TestContentValidation(t *testing.T) {
	good := baseContent(t)
	require.NoError(t, good.Validate())
	mut := map[string]func(*Content){
		"currency":      func(c *Content) { c.Currency = "usd" },
		"validity":      func(c *Content) { c.ValidityDate = "31/12/2026" },
		"discount>100":  func(c *Content) { c.DiscountPercent = kit.MustAmount("100.0001") },
		"discount<0":    func(c *Content) { c.DiscountPercent = kit.MustAmount("-1") },
		"price source":  func(c *Content) { c.PriceSourceRef = "has space" },
		"no lines":      func(c *Content) { c.Lines = nil },
		"tax no params": func(c *Content) { c.RequestTaxEstimate = true },
		"tax bad kind": func(c *Content) {
			c.RequestTaxEstimate = true
			c.TaxParams = &TaxParams{JurisdictionID: "GB", TaxCategory: "STANDARD", SupplyKind: "STUFF", SupplyType: "B2B"}
		},
	}
	for name, f := range mut {
		c := baseContent(t)
		f(&c)
		require.Error(t, c.Validate(), name)
	}
	c := baseContent(t)
	c.RequestTaxEstimate = true
	c.TaxParams = &TaxParams{JurisdictionID: "GB", TaxCategory: "STANDARD", SupplyKind: "SERVICES", SupplyType: "B2B"}
	require.NoError(t, c.Validate())
}
