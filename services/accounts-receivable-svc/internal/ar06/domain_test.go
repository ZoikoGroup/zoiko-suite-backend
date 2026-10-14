package ar06

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func TestStateMachine(t *testing.T) {
	for _, c := range []struct {
		from State
		tr   Transition
		to   State
	}{
		{StateDraft, TrSubmit, StatePendingApproval}, {StatePendingApproval, TrApprove, StateApproved}, {StateApproved, TrIssue, StateIssued},
		{StateApproved, TrInvalidate, StateDraft}, {StatePendingApproval, TrInvalidate, StateDraft},
		{StateDraft, TrCancel, StateCancelled}, {StatePendingApproval, TrCancel, StateCancelled}, {StateApproved, TrCancel, StateCancelled},
	} {
		got, err := Next(c.from, c.tr)
		require.NoError(t, err)
		require.Equal(t, c.to, got)
	}
	// illegal moves
	for _, c := range []struct {
		from State
		tr   Transition
	}{{StateDraft, TrApprove}, {StateDraft, TrIssue}, {StatePendingApproval, TrIssue}, {StateCancelled, TrSubmit}, {StateCancelled, TrIssue}} {
		_, err := Next(c.from, c.tr)
		require.Equal(t, kit.CodeInvalidTransition, err.(*kit.Problem).Code, "%s %s", c.from, c.tr)
	}
	// an issued note is immutable and never issued twice
	for _, s := range []State{StateIssued, StateApplied, StateSettled} {
		_, err := Next(s, TrIssue)
		require.Equal(t, CodeCorrectionAlreadyIssued, err.(*kit.Problem).Code)
		for _, tr := range []Transition{TrSubmit, TrApprove, TrCancel, TrInvalidate} {
			_, err = Next(s, tr)
			require.Equal(t, kit.CodeImmutable, err.(*kit.Problem).Code)
		}
	}
}

func TestEligibility(t *testing.T) {
	src := sourceInvoice("e", "p")
	e := EligibilityOf(src, kit.MustAmount("400"), kit.MustAmount("480"))
	require.Equal(t, kit.MustAmount("850.50"), e.RemainingNet)
	require.Equal(t, kit.MustAmount("1020.60"), e.RemainingGross)
	require.NoError(t, e.Check(kit.MustAmount("850.50"), kit.MustAmount("1020.60"), true))
	require.NoError(t, e.Check(kit.MustAmount("850.50"), 0, false))
	for _, c := range []struct {
		net, gross string
		have       bool
	}{{"850.51", "0", false}, {"100", "1020.61", true}, {"850.51", "1020.61", true}} {
		err := e.Check(kit.MustAmount(c.net), kit.MustAmount(c.gross), c.have)
		require.Equal(t, kit.CodeCorrectionExceedsEligible, err.(*kit.Problem).Code)
	}
}

func TestBuildLines(t *testing.T) {
	src := sourceInvoice("e", "p").Lines
	in := func(l LineInput) []LineInput { return []LineInput{l} }
	ok := LineInput{SourceLineNo: 1, Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("100.00")}
	lines, err := BuildLines(KindCredit, "USD", in(ok), src)
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("100.00"), lines[0].Net)
	require.Equal(t, "STANDARD", lines[0].TaxCategory, "tax category defaults from the source line")

	bad := map[string]LineInput{
		"credit without source line": {Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")},
		"no description":             {SourceLineNo: 1, Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")},
		"zero qty":                   {SourceLineNo: 1, Description: "x", Quantity: 0, UnitPrice: kit.MustAmount("1")},
		"negative price":             {SourceLineNo: 1, Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("-1")},
		"bad supply kind":            {SourceLineNo: 1, Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1"), SupplyKind: "STUFF"},
	}
	for name, l := range bad {
		_, err := BuildLines(KindCredit, "USD", in(l), src)
		require.Equal(t, kit.CodeValidationFailed, err.(*kit.Problem).Code, name)
	}
	_, err = BuildLines(KindCredit, "USD", in(LineInput{SourceLineNo: 9, Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("1")}), src)
	require.Equal(t, CodeCorrectionSourceInvalid, err.(*kit.Problem).Code, "a line naming a source line that does not exist is a wrong-source correction")
	_, err = BuildLines(KindCredit, "USD", in(LineInput{SourceLineNo: 2, Description: "x", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("250.51")}), src)
	require.Equal(t, kit.CodeCorrectionExceedsEligible, err.(*kit.Problem).Code, "a credit line cannot exceed its source line")
	_, err = BuildLines(KindDebit, "USD", in(LineInput{Description: "surcharge", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("5000")}), src)
	require.NoError(t, err, "a debit note line needs no source line and is not capped")
	_, err = BuildLines(KindCredit, "USD", nil, src)
	require.Error(t, err)
}

func TestContentValidate(t *testing.T) {
	good := Content{ReasonCode: "PRICE_ADJUSTMENT", SupplyDate: "2026-10-10"}
	require.NoError(t, good.Validate())
	bad := good
	bad.ReasonCode = " "
	require.Equal(t, kit.CodeReasonRequired, bad.Validate().(*kit.Problem).Code)
	bad = good
	bad.SupplyDate = "10/10/2026"
	require.Error(t, bad.Validate())
	bad = good
	bad.ClientReference = "has space"
	require.Error(t, bad.Validate())
	bad = good
	bad.Tax.SupplyType = "B2X"
	require.Error(t, bad.Validate())
}

func TestThresholds(t *testing.T) {
	require.False(t, EvidenceNeeded(KindCredit, kit.MustAmount("1000")))
	require.True(t, EvidenceNeeded(KindCredit, kit.MustAmount("1000.01")))
	require.False(t, EvidenceNeeded(KindDebit, kit.MustAmount("999999")))
	require.False(t, IsMaterial(kit.MustAmount("10000")))
	require.True(t, IsMaterial(kit.MustAmount("10000.01")))
}

func TestPostingLinesBalance(t *testing.T) {
	net, tax := kit.MustAmount("400.00"), kit.MustAmount("80.00")
	gross := net.Add(tax)
	for _, k := range []Kind{KindCredit, KindDebit} {
		ev := kit.PostingEvent{SourceEventID: "x", Lines: postingLines(k, "N-1", net, tax, gross)}
		require.NoError(t, ev.Validate(), k)
		keys := map[string]bool{}
		for _, l := range ev.Lines {
			keys[l.MappingKey] = true
		}
		require.True(t, keys[MappingReceivable] && keys[MappingTaxOutput], k)
	}
	cr := postingLines(KindCredit, "N", net, tax, gross)
	require.Equal(t, MappingCreditNoteRevenue, cr[0].MappingKey)
	require.Equal(t, net, cr[0].Debit, "credit note: Dr credit-note revenue (net)")
	require.Equal(t, gross, cr[len(cr)-1].Credit, "credit note: Cr receivable (gross)")
	dr := postingLines(KindDebit, "N", net, tax, gross)
	require.Equal(t, gross, dr[0].Debit, "debit note: Dr receivable (gross)")
	require.Equal(t, MappingRevenue, dr[1].MappingKey)
	// no tax line when there is no tax
	require.Len(t, postingLines(KindCredit, "N", net, 0, net), 2)
}

func TestNumberingFormat(t *testing.T) {
	require.Equal(t, "CN-000001", DefaultNumbering(KindCredit).Format(1))
	require.Equal(t, "DN-000042", DefaultNumbering(KindDebit).Format(42))
	require.Equal(t, "CN-1234567", DefaultNumbering(KindCredit).Format(1234567), "never truncated")
	require.NoError(t, DefaultNumbering(KindCredit).Validate())
	require.Error(t, NumberingProfile{Series: "bad series", Width: 6}.Validate())
}

func TestContentHashBindsEverything(t *testing.T) {
	h := &Header{CorrectionID: "c", LegalEntityID: "e", CustomerPartyID: "p", Kind: KindCredit, SourceInvoiceID: "s", Currency: "USD",
		Content: Content{ReasonCode: "R", SupplyDate: "2026-10-10"}}
	lines := []Line{{LineNo: 1, SourceLineNo: 1, Description: "d", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("10"), Net: kit.MustAmount("10")}}
	h0 := ContentHash(h, lines, nil)
	require.Equal(t, h0, ContentHash(h, lines, nil), "deterministic")
	mut := *h
	mut.SourceInvoiceID = "other"
	require.NotEqual(t, h0, ContentHash(&mut, lines, nil), "the source linkage is bound")
	mut = *h
	mut.Content.ReasonCode = "OTHER"
	require.NotEqual(t, h0, ContentHash(&mut, lines, nil))
	l2 := []Line{lines[0]}
	l2[0].Net = kit.MustAmount("11")
	require.NotEqual(t, h0, ContentHash(h, l2, nil))
	g := []ar04.TaxGroup{{GroupNo: 1, DeterminationID: "d1", Provenance: "p1", Tax: kit.MustAmount("2")}}
	require.NotEqual(t, h0, ContentHash(h, lines, g), "the tax result is bound")
	g2 := []ar04.TaxGroup{{GroupNo: 1, DeterminationID: "d1", Provenance: "p2", Tax: kit.MustAmount("2")}}
	require.NotEqual(t, ContentHash(h, lines, g), ContentHash(h, lines, g2), "the rule-pack provenance is bound")
}

func TestBuildIssuedIsCanonicalAndHashed(t *testing.T) {
	h := &Header{CorrectionID: "c", TenantID: "t", LegalEntityID: "e", CustomerPartyID: "p", Kind: KindDebit, SourceInvoiceID: "s", SourceInvoiceNumber: "INV-1", Currency: "USD",
		Content: Content{ReasonCode: "R", SupplyDate: "2026-10-10"}, NetTotal: kit.MustAmount("10"), GrossTotal: kit.MustAmount("10")}
	in := IssueInput{H: h, Number: "DN-000001", Series: "DN", Sequence: 1, IssueDate: "2026-10-12", DueDate: "2026-11-11", IssuedBy: "u", IssuedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	c1, h1, err := BuildIssued(in)
	require.NoError(t, err)
	c2, h2, _ := BuildIssued(in)
	require.Equal(t, c1, c2)
	require.Equal(t, h1, h2)
	require.Equal(t, h1, HashOf(c1))
	require.Contains(t, c1, SchemaIssuedNote)
	require.Contains(t, c1, `"source_invoice_id":"s"`)
}

func TestTaxAllocationPerGroup(t *testing.T) {
	lines := []Line{
		{LineNo: 1, Net: kit.MustAmount("100.00"), TaxCategory: "STANDARD", ProductClassification: "A", SupplyKind: "GOODS"},
		{LineNo: 2, Net: kit.MustAmount("50.00"), TaxCategory: "STANDARD", ProductClassification: "B", SupplyKind: "SERVICES"},
	}
	specs := BuildGroups(lines)
	require.Len(t, specs, 2, "one determination per tax group")
	groups := []ar04.TaxGroup{
		{GroupNo: 1, TaxCategory: "STANDARD", ProductClassification: "A", SupplyKind: "GOODS", Net: kit.MustAmount("100.00"), Tax: kit.MustAmount("20.00")},
		{GroupNo: 2, TaxCategory: "STANDARD", ProductClassification: "B", SupplyKind: "SERVICES", Net: kit.MustAmount("50.00"), Tax: kit.MustAmount("5.00")},
	}
	out, err := ApplyTax(lines, groups, "USD")
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("20.00"), out[0].Tax)
	require.Equal(t, kit.MustAmount("5.00"), out[1].Tax)
	require.Equal(t, kit.Amount(0), lines[0].Tax, "the input is not mutated")
	tot, err := TotalsOf(out, groups, "USD")
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("175.00"), tot.Gross)
}

// Spec negative path "original invoice edited instead of note": this module never touches another module's tables. Every SQL
// string in non-test source files may name only ar06_* / ar_* (kit) tables.
func TestNeverTouchesAnotherModulesTables(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, forbidden := range []string{"ar04_", "ar07_", "ar03_", "ar05_", "customer_invoices"} {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				require.NotContains(t, line, forbidden, "%s must reach other modules only through kit ports", f)
			}
		}
	}
}
