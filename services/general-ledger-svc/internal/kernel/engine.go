package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

// FXBasis is the exchange-rate evidence that converts a transaction amount into the book's functional currency. It is
// persisted with the proposal so the conversion can be reproduced (AK-INV-013). Where it comes from is the caller's
// concern (the currency registry and the FX policy); the engine only applies it exactly.
type FXBasis struct {
	Rate     string `json:"rate"` // functional units per one transaction unit
	RateType string `json:"rate_type"`
	Source   string `json:"source"`
	AsOf     string `json:"as_of"`
	RateID   string `json:"rate_id,omitempty"`
}

// AccountInfo is what the chart says about an account a rule line resolves to.
type AccountInfo struct {
	Code               string
	Postable           bool // header/parent accounts are not
	Active             bool
	CurrencyRestricted []string          // empty = any
	BookIDs            []string          // books the account is valid in; empty = every book
	// ControlType is the subledger or system area that owns the account (AR, AP, BANK, ...); empty for an ordinary account.
	// CounterpartyRequired makes a line on the account carry the counterparty (customer, supplier). The AR and AP control
	// accounts always do: a receivable or payable with no one it is owed by or to cannot be reconciled to its subledger.
	ControlType          string
	CounterpartyRequired bool
	DimensionPolicy    map[string]string // dimension -> REQUIRED | PROHIBITED | OPTIONAL
}

// Directory is the reference data an evaluation reads: currency precision, the chart and the dimension registry. It is
// an interface so the engine stays pure and every answer is part of the evidence the caller records.
type Directory interface {
	MinorUnits(ctx context.Context, currency string, asOf time.Time) (int, error)
	ResolveAccount(ctx context.Context, ref AccountRef, bookID string, asOf time.Time) (*AccountInfo, error)
	DimensionAllowed(ctx context.Context, dimension, value, legalEntityID, bookID string, asOf time.Time) (bool, error)
}

// EvalInput is everything one evaluation needs. Nothing is read from the clock or from a global.
type EvalInput struct {
	Event       *AccountingEvent
	Book        *Book
	Rule        *RuleSet
	PostingDate time.Time
	FX          *FXBasis
	Directory   Directory
}

// ProposalLine is one proposed journal line. Original and functional amounts are both kept.
type ProposalLine struct {
	LineNo           int               `json:"line_no"`
	LineRef          string            `json:"line_ref"`
	Side             string            `json:"side"`
	AccountCode      string            `json:"account_code"`
	AmountTx         string            `json:"amount_transaction"`
	Currency         string            `json:"transaction_currency"`
	AmountFunctional string            `json:"amount_functional"`
	Dimensions       map[string]string `json:"dimensions,omitempty"`
	Description      string            `json:"description,omitempty"`
	Rounding         bool              `json:"rounding_residual,omitempty"`
	RealizedFX       bool              `json:"realized_fx,omitempty"`
	CounterpartyID   string            `json:"counterparty_id,omitempty"`
	ControlType      string            `json:"control_type,omitempty"`
}

// Provenance identifies the rule that produced a proposal (AK-INV-008).
type Provenance struct {
	RuleSetID   string   `json:"posting_rule_set_id"`
	RuleName    string   `json:"rule_name"`
	RuleVersion string   `json:"rule_version"`
	ContentHash string   `json:"content_hash"`
	TestPackID  string   `json:"test_pack_id"`
	Branch      []string `json:"line_refs_generated"`
}

// TraceStep is one piece of calculation evidence.
type TraceStep struct {
	Step   string `json:"step"`
	Detail string `json:"detail"`
}

// Proposal is the dry-run result of an evaluation: a balanced set of lines and why they are what they are. It changes
// nothing.
type Proposal struct {
	AccountingEventID string         `json:"accounting_event_id"`
	EventPayloadHash  string         `json:"event_payload_hash"`
	BookID            string         `json:"book_id"`
	LegalEntityID     string         `json:"legal_entity_id"`
	PostingDate       string         `json:"posting_date"`
	TransactionCcy    string         `json:"transaction_currency"`
	FunctionalCcy     string         `json:"functional_currency"`
	Lines             []ProposalLine `json:"lines"`
	TotalDebitTx      string         `json:"total_debit_transaction"`
	TotalCreditTx     string         `json:"total_credit_transaction"`
	TotalDebitFunc    string         `json:"total_debit_functional"`
	TotalCreditFunc   string         `json:"total_credit_functional"`
	FX                *FXBasis       `json:"fx_basis,omitempty"`
	RoundingPolicyID  string         `json:"rounding_policy_id"`
	RoundingMode      string         `json:"rounding_mode"`
	Provenance        Provenance     `json:"provenance"`
	Trace             []TraceStep    `json:"trace"`
	ProposalHash      string         `json:"proposal_hash"`
}

type rawLine struct {
	t      LineTemplate
	minor  *big.Int // transaction minor units, never negative
	acct   *AccountInfo
	dims   map[string]string
	exact  *big.Rat
	funcMn *big.Int
	round  bool
	cp     string
	// fixed is a functional amount the rule stated (carrying basis), exempt from conversion; realized marks the realized-FX line.
	fixed    *big.Rat
	realized bool
}

// Evaluate turns one event into a journal proposal under one rule set and one book. The same input always gives the same
// output byte for byte; every refusal is an *Exception naming its family.
func Evaluate(ctx context.Context, in EvalInput) (*Proposal, error) {
	ev, bk, rs := in.Event, in.Book, in.Rule
	var trace []TraceStep
	step := func(s, d string) { trace = append(trace, TraceStep{s, d}) }

	if ev.TenantID != bk.TenantID || ev.LegalEntityID != bk.LegalEntityID {
		return nil, exc(FamAuth, "ENTITY_MISMATCH", "the event and the book belong to different legal entities").with("event_entity", ev.LegalEntityID).with("book_entity", bk.LegalEntityID)
	}
	if !bk.InForce(in.PostingDate) {
		return nil, exc(FamRule, "BOOK_NOT_IN_FORCE", "book %s is %s on %s", bk.BookCode, strings.ToLower(bk.Status), in.PostingDate.Format("2006-01-02"))
	}
	if rs.EventType != ev.EventType || rs.BookType != bk.BookType || rs.FrameworkID != bk.AccountingFrameworkID {
		return nil, exc(FamRule, "RULE_NOT_APPLICABLE", "the rule set is for %s / %s / %s", rs.EventType, rs.BookType, rs.FrameworkID)
	}
	if !rs.Covers(in.PostingDate) {
		return nil, exc(FamRule, "RULE_NOT_IN_FORCE", "rule set %s %s is %s on %s", rs.Name, rs.Version, strings.ToLower(rs.Status), in.PostingDate.Format("2006-01-02"))
	}
	c := rs.Content
	if c.TaxDependency != nil {
		for _, f := range c.TaxDependency.RequiredFacts {
			if _, ok := ev.Facts[f]; !ok {
				return nil, exc(FamTax, "TAX_RESULT_MISSING", "the event depends on the authoritative tax result %q and does not carry it", f).with("fact", f)
			}
		}
		step("tax", "authoritative tax facts present")
	}
	if !evalConditions(ev, c.Conditions) {
		return nil, exc(FamRule, "NO_MATCHING_BRANCH", "the event does not satisfy the conditions of rule set %s %s", rs.Name, rs.Version)
	}
	step("rule", rs.Name+" "+rs.Version+" selected for "+ev.EventType)

	tccy := ev.TransactionCurrency
	if tccy == "" {
		tccy = bk.FunctionalCurrency
	}
	tscale, err := in.Directory.MinorUnits(ctx, tccy, in.PostingDate)
	if err != nil {
		return nil, exc(FamExt, "CURRENCY_UNAVAILABLE", "the precision of %s could not be read: %v", tccy, err)
	}
	fscale := tscale
	if bk.FunctionalCurrency != tccy {
		if fscale, err = in.Directory.MinorUnits(ctx, bk.FunctionalCurrency, in.PostingDate); err != nil {
			return nil, exc(FamExt, "CURRENCY_UNAVAILABLE", "the precision of %s could not be read: %v", bk.FunctionalCurrency, err)
		}
	}
	pol := BuiltinRoundingPolicies[c.RoundingPolicyID]
	step("rounding", pol.ID+" ("+pol.Mode+") applied once per line, at the minor unit of "+tccy)

	env := map[string]*big.Rat{}
	for _, k := range sortedKeys(ev.Facts) {
		if r, ok := ParseDecimal(ev.Facts[k]); ok {
			env[k] = r
			env["facts."+k] = r
		}
	}

	var lines []*rawLine
	for _, t := range c.LineTemplates {
		if !evalConditions(ev, t.When) {
			step("skip", t.LineRef+": its condition is not met")
			continue
		}
		expr, perr := ParseExpr(t.Amount)
		if perr != nil {
			return nil, perr
		}
		exact, eerr := expr.Eval(env)
		if eerr != nil {
			return nil, eerr
		}
		if exact.Sign() < 0 {
			return nil, exc(FamData, "NEGATIVE_AMOUNT", "line %s computes a negative amount (%s): a rule states positive amounts and chooses the side", t.LineRef, ratString(exact))
		}
		minor := RoundMinor(exact, tscale, pol.Mode)
		step("line", t.LineRef+": "+t.Amount+" = "+ratString(exact)+" -> "+FormatMinor(minor, tscale))
		if minor.Sign() == 0 && !t.KeepZero {
			continue
		}
		info, aerr := in.Directory.ResolveAccount(ctx, t.Account, bk.BookID, in.PostingDate)
		if aerr != nil {
			return nil, exc(FamCOA, "ACCOUNT_UNRESOLVED", "%s (%s): %v", t.LineRef, t.Account, aerr).with("line_ref", t.LineRef)
		}
		if info == nil || !info.Active || !info.Postable {
			return nil, exc(FamCOA, "ACCOUNT_NOT_POSTABLE", "%s resolves to an account that is inactive or a header account", t.LineRef).with("line_ref", t.LineRef)
		}
		if len(info.BookIDs) > 0 && !containsStr(info.BookIDs, bk.BookID) {
			return nil, exc(FamCOA, "ACCOUNT_NOT_IN_BOOK", "account %s is not valid in book %s", info.Code, bk.BookCode).with("line_ref", t.LineRef)
		}
		if len(info.CurrencyRestricted) > 0 && !containsStr(info.CurrencyRestricted, tccy) {
			return nil, exc(FamCOA, "ACCOUNT_CURRENCY_RESTRICTED", "account %s does not take %s", info.Code, tccy).with("line_ref", t.LineRef)
		}
		dims, derr := resolveDimensions(ctx, in, c, t, info)
		if derr != nil {
			return nil, derr
		}
		cp := ""
		if info.CounterpartyRequired || info.ControlType == "AR" || info.ControlType == "AP" {
			if ev.CounterpartyID == "" {
				return nil, exc(FamCOA, "COUNTERPARTY_REQUIRED", "%s posts to account %s, which needs the counterparty and the event carries none", t.LineRef, info.Code).with("line_ref", t.LineRef)
			}
			cp = ev.CounterpartyID
		}
		var fixed *big.Rat
		if t.Functional != "" {
			fexpr, ferr := ParseExpr(t.Functional)
			if ferr != nil {
				return nil, ferr
			}
			if fixed, ferr = fexpr.Eval(env); ferr != nil {
				return nil, ferr
			}
			if fixed.Sign() < 0 {
				return nil, exc(FamData, "NEGATIVE_AMOUNT", "line %s computes a negative functional amount (%s)", t.LineRef, ratString(fixed))
			}
			step("carrying", t.LineRef+": functional "+t.Functional+" = "+ratString(fixed)+" (retired at its carrying amount)")
		}
		lines = append(lines, &rawLine{t: t, minor: minor, acct: info, dims: dims, exact: exact, cp: cp, fixed: fixed})
	}
	if len(lines) < 2 {
		return nil, exc(FamRule, "NO_LINES", "the rule produced fewer than two lines for this event")
	}

	// transaction-currency balance
	dr, cr := new(big.Int), new(big.Int)
	for _, l := range lines {
		if l.t.Side == Debit {
			dr.Add(dr, l.minor)
		} else {
			cr.Add(cr, l.minor)
		}
	}
	if dr.Cmp(cr) != 0 {
		if err := addResidual(ctx, in, c, pol, &lines, dr, cr, tccy, false); err != nil {
			return nil, err
		}
		step("residual", "a transaction-currency difference of "+FormatMinor(new(big.Int).Abs(new(big.Int).Sub(dr, cr)), tscale)+" was posted to the rounding account")
		dr, cr = new(big.Int), new(big.Int)
		for _, l := range lines {
			if l.t.Side == Debit {
				dr.Add(dr, l.minor)
			} else {
				cr.Add(cr, l.minor)
			}
		}
	}

	// functional currency
	var fx *FXBasis
	rate := big.NewRat(1, 1)
	if bk.FunctionalCurrency != tccy {
		if in.FX == nil {
			return nil, exc(FamFX, "RATE_MISSING", "a %s event in a %s book needs an exchange rate and none was supplied", tccy, bk.FunctionalCurrency)
		}
		r, ok := ParseDecimal(in.FX.Rate)
		if !ok || r.Sign() <= 0 {
			return nil, exc(FamFX, "RATE_INVALID", "the exchange rate %q is not a positive decimal", in.FX.Rate)
		}
		if in.FX.Source == "" || in.FX.RateType == "" || in.FX.AsOf == "" {
			return nil, exc(FamFX, "RATE_UNSOURCED", "an exchange rate carries its type, source and date")
		}
		rate, fx = r, in.FX
		step("fx", tccy+"->"+bk.FunctionalCurrency+" at "+in.FX.Rate+" ("+in.FX.RateType+", "+in.FX.Source+", "+in.FX.AsOf+")")
	}
	fdr, fcr := new(big.Int), new(big.Int)
	for _, l := range lines {
		if l.funcMn != nil {
			// a residual line already carries its functional amount
		} else if l.fixed != nil {
			l.funcMn = RoundMinor(l.fixed, fscale, pol.Mode)
		} else if bk.FunctionalCurrency == tccy {
			l.funcMn = new(big.Int).Set(l.minor)
		} else {
			amt := new(big.Rat).SetFrac(l.minor, pow10(tscale))
			l.funcMn = RoundMinor(amt.Mul(amt, rate), fscale, pol.Mode)
		}
		if l.t.Side == Debit {
			fdr.Add(fdr, l.funcMn)
		} else {
			fcr.Add(fcr, l.funcMn)
		}
	}
	anyFixed := false
	for _, l := range lines {
		anyFixed = anyFixed || l.fixed != nil
	}
	if fdr.Cmp(fcr) != 0 && anyFixed && c.RealizedFX != nil {
		if err := addRealizedFX(ctx, in, c, &lines, fdr, fcr); err != nil {
			return nil, err
		}
		step("realized-fx", "the difference between the carrying amount retired and the value settled was posted as realized exchange difference")
		fdr, fcr = new(big.Int), new(big.Int)
		for _, l := range lines {
			if l.t.Side == Debit {
				fdr.Add(fdr, l.funcMn)
			} else {
				fcr.Add(fcr, l.funcMn)
			}
		}
	}
	if fdr.Cmp(fcr) != 0 {
		if err := addResidual(ctx, in, c, pol, &lines, fdr, fcr, bk.FunctionalCurrency, true); err != nil {
			return nil, err
		}
		step("residual", "a functional-currency difference was posted to the rounding account")
		fdr, fcr = new(big.Int), new(big.Int)
		for _, l := range lines {
			if l.t.Side == Debit {
				fdr.Add(fdr, l.funcMn)
			} else {
				fcr.Add(fcr, l.funcMn)
			}
		}
	}
	if fdr.Cmp(fcr) != 0 || dr.Cmp(cr) != 0 {
		return nil, exc(FamBal, "UNBALANCED", "debits do not equal credits after rounding")
	}

	p := &Proposal{AccountingEventID: ev.AccountingEventID, EventPayloadHash: ev.PayloadHash, BookID: bk.BookID, LegalEntityID: bk.LegalEntityID,
		PostingDate: in.PostingDate.Format("2006-01-02"), TransactionCcy: tccy, FunctionalCcy: bk.FunctionalCurrency, FX: fx,
		RoundingPolicyID: pol.ID, RoundingMode: pol.Mode,
		TotalDebitTx: FormatMinor(dr, tscale), TotalCreditTx: FormatMinor(cr, tscale), TotalDebitFunc: FormatMinor(fdr, fscale), TotalCreditFunc: FormatMinor(fcr, fscale)}
	prov := Provenance{RuleSetID: rs.RuleSetID, RuleName: rs.Name, RuleVersion: rs.Version, ContentHash: rs.ContentHash, TestPackID: rs.TestPackID}
	for i, l := range lines {
		prov.Branch = append(prov.Branch, l.t.LineRef)
		p.Lines = append(p.Lines, ProposalLine{LineNo: i + 1, LineRef: l.t.LineRef, Side: l.t.Side, AccountCode: l.acct.Code,
			AmountTx: FormatMinor(l.minor, tscale), Currency: tccy, AmountFunctional: FormatMinor(l.funcMn, fscale), Dimensions: l.dims,
			Description: l.t.Description, Rounding: l.round, RealizedFX: l.realized, CounterpartyID: l.cp, ControlType: l.acct.ControlType})
	}
	p.Provenance, p.Trace = prov, trace
	b, _ := json.Marshal(struct {
		E, B, D string
		L       []ProposalLine
		P       Provenance
		F       *FXBasis
	}{ev.PayloadHash, bk.BookID, p.PostingDate, p.Lines, prov, fx})
	sum := sha256.Sum256(b)
	p.ProposalHash = hex.EncodeToString(sum[:])
	return p, nil
}

// addRealizedFX posts the functional difference of a settlement as an explicit realized exchange gain or loss: more debit than
// credit needs a credit, which is a gain, and the reverse is a loss. The line carries no transaction amount: it is a pure
// functional-currency difference.
func addRealizedFX(ctx context.Context, in EvalInput, c RuleContent, lines *[]*rawLine, dr, cr *big.Int) error {
	diff := new(big.Int).Sub(dr, cr)
	ref, side := c.RealizedFX.Gain, Credit
	if diff.Sign() < 0 {
		ref, side = c.RealizedFX.Loss, Debit
	}
	info, err := in.Directory.ResolveAccount(ctx, ref, in.Book.BookID, in.PostingDate)
	if err != nil || info == nil || !info.Active || !info.Postable {
		return exc(FamCOA, "REALIZED_FX_ACCOUNT_UNAVAILABLE", "the realized exchange account %s cannot take the difference", ref)
	}
	*lines = append(*lines, &rawLine{t: LineTemplate{LineRef: "REALIZED_FX", Side: side, Description: "realized exchange difference"}, acct: info, realized: true,
		minor: new(big.Int), funcMn: new(big.Int).Abs(diff), dims: map[string]string{}})
	return nil
}

// addResidual posts a difference within the rounding policy's tolerance to the rule's rounding account as an explicit
// line; anything larger, or a policy that allows none, is an unbalanced journal (standard §8.3, §35).
func addResidual(ctx context.Context, in EvalInput, c RuleContent, pol RoundingPolicy, lines *[]*rawLine, dr, cr *big.Int, ccy string, functional bool) error {
	diff := new(big.Int).Sub(dr, cr)
	abs := new(big.Int).Abs(diff)
	if pol.ResidualTolerance == 0 || c.RoundingAccount == nil || abs.Cmp(big.NewInt(int64(pol.ResidualTolerance))) > 0 {
		return exc(FamBal, "UNBALANCED", "debits and credits differ by %s minor units of %s and the rounding policy %s allows %d", abs.String(), ccy, pol.ID, pol.ResidualTolerance)
	}
	info, err := in.Directory.ResolveAccount(ctx, *c.RoundingAccount, in.Book.BookID, in.PostingDate)
	if err != nil || info == nil || !info.Active || !info.Postable {
		return exc(FamCOA, "ROUNDING_ACCOUNT_UNAVAILABLE", "the rounding account %s cannot take a residual", c.RoundingAccount)
	}
	side := Credit
	if diff.Sign() < 0 {
		side = Debit
	}
	l := &rawLine{t: LineTemplate{LineRef: "ROUNDING", Side: side, Description: "rounding residual"}, acct: info, round: true, minor: new(big.Int), dims: map[string]string{}}
	if functional {
		l.funcMn = abs
		l.minor = new(big.Int)
	} else {
		l.minor = abs
	}
	*lines = append(*lines, l)
	return nil
}

func resolveDimensions(ctx context.Context, in EvalInput, c RuleContent, t LineTemplate, info *AccountInfo) (map[string]string, error) {
	ev := in.Event
	dims := map[string]string{}
	pick := func(src string) string {
		switch {
		case strings.HasPrefix(src, "$dimensions."):
			return ev.Dimensions[strings.TrimPrefix(src, "$dimensions.")]
		case strings.HasPrefix(src, "$facts."):
			return ev.Facts[strings.TrimPrefix(src, "$facts.")]
		}
		return src
	}
	for _, d := range sortedKeys(t.Dimensions) {
		dims[d] = pick(t.Dimensions[d])
	}
	for _, r := range c.DimensionRules {
		switch r.Mode {
		case DimDerived:
			if dims[r.Dimension] == "" {
				dims[r.Dimension] = pick(r.From)
			}
		case DimProhibited:
			if dims[r.Dimension] != "" {
				return nil, exc(FamDim, "DIMENSION_PROHIBITED", "dimension %s is prohibited by the rule on line %s", r.Dimension, t.LineRef).with("line_ref", t.LineRef)
			}
		case DimRequired:
			if dims[r.Dimension] == "" {
				if v, ok := ev.Dimensions[r.Dimension]; ok && v != "" {
					dims[r.Dimension] = v
				} else {
					return nil, exc(FamDim, "DIMENSION_MISSING", "dimension %s is required on line %s", r.Dimension, t.LineRef).with("line_ref", t.LineRef)
				}
			}
		case DimOptional:
			if dims[r.Dimension] == "" {
				if v := ev.Dimensions[r.Dimension]; v != "" {
					dims[r.Dimension] = v
				}
			}
		}
	}
	for d, pol := range info.DimensionPolicy {
		switch pol {
		case DimRequired:
			if dims[d] == "" {
				return nil, exc(FamDim, "DIMENSION_MISSING", "account %s requires dimension %s", info.Code, d).with("line_ref", t.LineRef)
			}
		case DimProhibited:
			if dims[d] != "" {
				return nil, exc(FamDim, "DIMENSION_PROHIBITED", "account %s prohibits dimension %s", info.Code, d).with("line_ref", t.LineRef)
			}
		}
	}
	for _, d := range sortedKeys(dims) {
		if dims[d] == "" {
			delete(dims, d)
			continue
		}
		ok, err := in.Directory.DimensionAllowed(ctx, d, dims[d], in.Event.LegalEntityID, in.Book.BookID, in.PostingDate)
		if err != nil {
			return nil, exc(FamExt, "DIMENSION_REGISTRY_UNAVAILABLE", "dimension %s could not be checked: %v", d, err)
		}
		if !ok {
			return nil, exc(FamDim, "DIMENSION_VALUE_INVALID", "%s=%s is not active for this entity, book and date", d, dims[d]).with("line_ref", t.LineRef)
		}
	}
	return dims, nil
}

func containsStr(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// RunTestPack runs a rule set's regression cases through the engine. It returns the names of cases that failed and why;
// an empty result means the pack passed. The engine is the same one production uses, over a directory the case defines.
func RunTestPack(ctx context.Context, rs *RuleSet, book Book) []string {
	var failed []string
	for _, tc := range rs.TestPack {
		bk := book
		if tc.Functional != "" {
			bk.FunctionalCurrency = tc.Functional
		}
		ev := &AccountingEvent{AccountingEventID: "test", TenantID: bk.TenantID, LegalEntityID: bk.LegalEntityID, EventType: tc.Event.EventType,
			TransactionCurrency: tc.Event.TransactionCurrency, Facts: tc.Event.Facts, Dimensions: tc.Event.Dimensions, CounterpartyID: tc.Event.CounterpartyID}
		dir := testDirectory{accounts: tc.Accounts, units: tc.MinorUnits, controls: tc.Controls}
		covered := *rs
		covered.Status = RuleActive
		p, err := Evaluate(ctx, EvalInput{Event: ev, Book: &bk, Rule: &covered, PostingDate: rs.EffectiveFrom, FX: tc.FX, Directory: dir})
		switch {
		case tc.Expect.Exception != "":
			if e, ok := AsException(err); !ok || e.Family != tc.Expect.Exception {
				failed = append(failed, tc.Name+": expected exception "+tc.Expect.Exception+" and got "+errString(err))
			}
		case err != nil:
			failed = append(failed, tc.Name+": "+err.Error())
		default:
			if msg := compareLines(p.Lines, tc.Expect.Lines); msg != "" {
				failed = append(failed, tc.Name+": "+msg)
			}
		}
	}
	return failed
}

func errString(err error) string {
	if err == nil {
		return "a proposal"
	}
	return err.Error()
}

func compareLines(got []ProposalLine, want []ExpectedLine) string {
	if len(got) != len(want) {
		return "expected " + itoa(len(want)) + " lines and got " + itoa(len(got))
	}
	for i := range got {
		if got[i].Side != want[i].Side || got[i].AccountCode != want[i].AccountCode || got[i].AmountTx != want[i].Amount || (want[i].Functional != "" && got[i].AmountFunctional != want[i].Functional) {
			return "line " + itoa(i+1) + " is " + got[i].Side + " " + got[i].AccountCode + " " + got[i].AmountTx + ", expected " + want[i].Side + " " + want[i].AccountCode + " " + want[i].Amount
		}
	}
	return ""
}

func itoa(n int) string { return new(big.Int).SetInt64(int64(n)).String() }

// testDirectory resolves a test case's own accounts: every account is active and postable, every dimension value valid.
type testDirectory struct {
	accounts map[string]string
	units    map[string]int
	controls map[string]string
}

func (d testDirectory) MinorUnits(_ context.Context, ccy string, _ time.Time) (int, error) {
	if u, ok := d.units[ccy]; ok {
		return u, nil
	}
	return 2, nil
}

func (d testDirectory) ResolveAccount(_ context.Context, ref AccountRef, _ string, _ time.Time) (*AccountInfo, error) {
	code := ref.AccountCode
	if ref.MappingKey != "" {
		code = d.accounts[ref.MappingKey]
		if code == "" {
			return nil, exc(FamCOA, "MAPPING_MISSING", "no account for mapping %q", ref.MappingKey)
		}
	}
	return &AccountInfo{Code: code, Postable: true, Active: true, ControlType: d.controls[code]}, nil
}

func (d testDirectory) DimensionAllowed(context.Context, string, string, string, string, time.Time) (bool, error) {
	return true, nil
}
