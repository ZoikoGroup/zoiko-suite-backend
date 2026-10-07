package domain

import (
	"fmt"
	"math/big"
	"sort"
	"time"
)

// MatchAlgorithmVersion is recorded in every evidence package (Invariant 16:
// automated controls must prove the algorithm that produced the result).
const MatchAlgorithmVersion = "exact-reference-match/1"

// Exception categories — the §21 taxonomy — and the assertion each impacts.
const (
	CatMissing   = "MISSING"
	CatDuplicate = "DUPLICATE"
	CatMismatch  = "MISMATCH"
	CatTiming    = "TIMING"
)

// Reason codes give the category its specific cause.
const (
	ReasonMissingDownstream = "MISSING_DOWNSTREAM" // side A record has no side B counterpart
	ReasonMissingSource     = "MISSING_SOURCE"     // side B record has no side A counterpart
	ReasonAmountMismatch    = "AMOUNT_MISMATCH"
	ReasonCurrencyMismatch  = "CURRENCY_MISMATCH"
	ReasonDateOutOfTol      = "DATE_OUTSIDE_TOLERANCE"
	ReasonDuplicateID       = "DUPLICATE_RECORD_ID"
	ReasonDuplicateContent  = "DUPLICATE_CONTENT"
	ReasonAmbiguousKey      = "AMBIGUOUS_MATCH_KEY"
	ReasonOutstandingItem   = "OUTSTANDING_ITEM" // reconciling item with an expected clearing date
	ReasonGroupMismatch     = "GROUP_AMOUNT_MISMATCH"
)

// MatchParams are the PINNED comparison parameters. They come from the run's
// pinned tolerance policy, never from the request (Invariant 4).
type MatchParams struct {
	AbsoluteTolerance *big.Rat // difference allowed on an already-paired amount
	DateToleranceDays int

	// From the pinned RULE version (not the request):
	AllowGroups           bool      // reconcile shared-reference groups as an explicit allocation
	SkipContentDuplicates bool      // see RuleLogic.SkipContentDuplicates
	OutstandingDays       int       // unmatched items this close to period end are reconciling items
	PeriodEnd             time.Time // zero disables outstanding-item handling
}

// MatchOutcome classifies a paired record.
type MatchOutcome string

const (
	OutcomeExact           MatchOutcome = "EXACT"
	OutcomeWithinTolerance MatchOutcome = "WITHIN_TOLERANCE"
	OutcomeGroupExact      MatchOutcome = "GROUP_EXACT"
	OutcomeGroupTolerance  MatchOutcome = "GROUP_WITHIN_TOLERANCE"
)

// Match kinds.
const (
	MatchOneToOne = "ONE_TO_ONE"
	MatchGroup    = "GROUP"
)

// MatchResult is one record-level pairing (§6 MatchResult).
type MatchResult struct {
	Kind         string       `json:"kind"`
	Reference    string       `json:"reference"`
	SideARecords []string     `json:"side_a_record_ids"`
	SideBRecords []string     `json:"side_b_record_ids"`
	SideA        string       `json:"side_a_record_id"` // first (or only) record; kept for 1:1 readers
	SideB        string       `json:"side_b_record_id"`
	Rule         string       `json:"match_rule"`
	Outcome      MatchOutcome `json:"outcome"`
	Difference   string       `json:"difference"` // A - B, exact decimal
	Currency     string       `json:"currency"`
}

// FoundException is an exception the engine derived; persistence adds identity,
// owner, due date and state.
type FoundException struct {
	Category  string   `json:"category"`
	Reason    string   `json:"reason_code"`
	Assertion string   `json:"assertion"`
	Side      Side     `json:"side"`
	RecordIDs []string `json:"record_ids"`
	Exposure  string   `json:"exposure"` // absolute value, exact decimal
	Currency  string   `json:"currency"`
	Detail    string   `json:"detail"`
	// ExpectedClearing is set for reconciling items: the date the item is expected
	// to clear, which becomes the exception due date.
	ExpectedClearing *time.Time `json:"expected_clearing,omitempty"`
}

// MatchOutput is the complete deterministic result of one execution.
type MatchOutput struct {
	Matches      []MatchResult    `json:"matches"`
	Exceptions   []FoundException `json:"exceptions"`
	MatchedCount int              `json:"matched_count"`
	// TotalsAgree reports whether headline control totals tie. It is
	// informational: a run can NEVER pass on totals alone (scenario 03).
	TotalsAgree bool `json:"totals_agree"`
}

// Result derives the financial result from the record-level findings — and only
// from them. Zero exceptions is the sole route to PASS.
func (o MatchOutput) Result() ResultState {
	if len(o.Exceptions) == 0 {
		return ResultPass
	}
	return ResultFail
}

// MatchPopulations compares side A against side B deterministically.
//
// Order of operations (§10): duplicates are detected FIRST and excluded from
// pairing; remaining records are paired by exact reference; tolerance applies
// only to a pair that already exists — it can never turn a missing or
// unauthorized record into a valid one (Invariant 5). Anything not cleanly
// paired becomes an item-level exception with exposure, so two populations
// whose totals happen to agree but whose composition differs still fail
// (scenario 03).
func MatchPopulations(a, b []PopulationRecord, p MatchParams) (MatchOutput, error) {
	tol := p.AbsoluteTolerance
	if tol == nil {
		tol = new(big.Rat)
	}
	var out MatchOutput

	cleanA, dupA, err := SplitDuplicatesOpt(a, p.SkipContentDuplicates)
	if err != nil {
		return out, err
	}
	cleanB, dupB, err := SplitDuplicatesOpt(b, p.SkipContentDuplicates)
	if err != nil {
		return out, err
	}
	for _, g := range dupA {
		out.Exceptions = append(out.Exceptions, duplicateExceptions(SideA, g)...)
	}
	for _, g := range dupB {
		out.Exceptions = append(out.Exceptions, duplicateExceptions(SideB, g)...)
	}

	// Group the clean records by reference.
	groupA, groupB := groupByReference(cleanA), groupByReference(cleanB)
	refs := unionKeys(groupA, groupB)

	for _, ref := range refs {
		as, bs := groupA[ref], groupB[ref]
		switch {
		case len(as) > 0 && len(bs) == 0:
			for _, r := range as {
				out.Exceptions = append(out.Exceptions, unmatchedItem(SideA, r, ReasonMissingDownstream, "COMPLETENESS",
					"present in the source population but absent downstream", p))
			}
		case len(as) == 0 && len(bs) > 0:
			for _, r := range bs {
				out.Exceptions = append(out.Exceptions, unmatchedItem(SideB, r, ReasonMissingSource, "EXISTENCE",
					"present downstream with no authoritative source record", p))
			}
		case len(as) == 1 && len(bs) == 1:
			m, ex, err := pair(as[0], bs[0], tol, p.DateToleranceDays)
			if err != nil {
				return out, err
			}
			if m != nil {
				out.Matches = append(out.Matches, *m)
				out.MatchedCount++
			}
			out.Exceptions = append(out.Exceptions, ex...)
		case p.AllowGroups:
			m, ex, err := groupMatch(ref, as, bs, tol)
			if err != nil {
				return out, err
			}
			if m != nil {
				out.Matches = append(out.Matches, *m)
				out.MatchedCount++
			}
			out.Exceptions = append(out.Exceptions, ex...)
		default:
			// Several records share one reference and grouping is not enabled for
			// this control. Guessing an allocation would let a wrong pairing hide a
			// difference, so the engine refuses to pair and raises one exception per
			// record.
			for _, r := range as {
				out.Exceptions = append(out.Exceptions, ambiguous(SideA, r, ref))
			}
			for _, r := range bs {
				out.Exceptions = append(out.Exceptions, ambiguous(SideB, r, ref))
			}
		}
	}

	totalsA, err := ComputeTotals(a)
	if err != nil {
		return out, err
	}
	totalsB, err := ComputeTotals(b)
	if err != nil {
		return out, err
	}
	out.TotalsAgree = sameTotals(totalsA, totalsB)

	sort.SliceStable(out.Exceptions, func(i, j int) bool {
		x, y := out.Exceptions[i], out.Exceptions[j]
		if x.Reason != y.Reason {
			return x.Reason < y.Reason
		}
		if x.Side != y.Side {
			return x.Side < y.Side
		}
		return x.RecordIDs[0] < y.RecordIDs[0]
	})
	sort.SliceStable(out.Matches, func(i, j int) bool { return out.Matches[i].SideA < out.Matches[j].SideA })
	return out, nil
}

func pair(a, b PopulationRecord, tol *big.Rat, dateDays int) (*MatchResult, []FoundException, error) {
	amtA, err := a.ParsedAmount()
	if err != nil {
		return nil, nil, err
	}
	amtB, err := b.ParsedAmount()
	if err != nil {
		return nil, nil, err
	}
	// A different currency is never "close enough": no FX is applied without an
	// approved rate basis (§10), and tolerance is per currency.
	if a.Currency != b.Currency {
		ex := FoundException{Category: CatMismatch, Reason: ReasonCurrencyMismatch, Assertion: "ACCURACY",
			Side: SideA, RecordIDs: []string{a.RecordID, b.RecordID},
			Exposure: CanonicalAmount(new(big.Rat).Abs(amtA)), Currency: a.Currency,
			Detail: fmt.Sprintf("currency %s vs %s for reference %q", a.Currency, b.Currency, a.Reference)}
		return nil, []FoundException{ex}, nil
	}
	diff := new(big.Rat).Sub(amtA, amtB)
	absDiff := new(big.Rat).Abs(diff)
	if absDiff.Cmp(tol) > 0 {
		ex := FoundException{Category: CatMismatch, Reason: ReasonAmountMismatch, Assertion: "ACCURACY",
			Side: SideA, RecordIDs: []string{a.RecordID, b.RecordID},
			Exposure: CanonicalAmount(absDiff), Currency: a.Currency,
			Detail: fmt.Sprintf("amount %s vs %s (difference %s) exceeds the pinned tolerance %s",
				CanonicalAmount(amtA), CanonicalAmount(amtB), CanonicalAmount(diff), CanonicalAmount(tol))}
		return nil, []FoundException{ex}, nil
	}
	// Amount is acceptable; date is a separate, explicit tolerance (timing differences are
	// reconciling items, not silently absorbed).
	da, _ := time.Parse("2006-01-02", a.Date)
	db, _ := time.Parse("2006-01-02", b.Date)
	days := int(da.Sub(db).Hours() / 24)
	if days < 0 {
		days = -days
	}
	if days > dateDays {
		ex := FoundException{Category: CatTiming, Reason: ReasonDateOutOfTol, Assertion: "CUTOFF",
			Side: SideA, RecordIDs: []string{a.RecordID, b.RecordID},
			Exposure: CanonicalAmount(new(big.Rat).Abs(amtA)), Currency: a.Currency,
			Detail: fmt.Sprintf("dates %s vs %s differ by %d days; tolerance is %d", a.Date, b.Date, days, dateDays)}
		return nil, []FoundException{ex}, nil
	}
	outcome := OutcomeExact
	if diff.Sign() != 0 {
		outcome = OutcomeWithinTolerance
	}
	return &MatchResult{Kind: MatchOneToOne, Reference: a.Reference, SideARecords: []string{a.RecordID}, SideBRecords: []string{b.RecordID},
		SideA: a.RecordID, SideB: b.RecordID, Rule: MatchAlgorithmVersion, Outcome: outcome,
		Difference: CanonicalAmount(diff), Currency: a.Currency}, nil, nil
}

func duplicateExceptions(side Side, g DuplicateGroup) []FoundException {
	reason := ReasonDuplicateContent
	if g.Kind == "RECORD_ID" {
		reason = ReasonDuplicateID
	}
	var out []FoundException
	for _, d := range g.Dupes {
		amt, _ := d.ParsedAmount()
		abs := new(big.Rat)
		if amt != nil {
			abs.Abs(amt)
		}
		out = append(out, FoundException{Category: CatDuplicate, Reason: reason, Assertion: "COMPLETENESS",
			Side: side, RecordIDs: []string{d.RecordID}, Exposure: CanonicalAmount(abs), Currency: d.Currency,
			Detail: fmt.Sprintf("duplicates record %q (reference %q)", g.Keep.RecordID, g.Keep.Reference)})
	}
	return out
}

func missing(side Side, r PopulationRecord, reason, assertion, detail string) FoundException {
	amt, _ := r.ParsedAmount()
	abs := new(big.Rat)
	if amt != nil {
		abs.Abs(amt)
	}
	return FoundException{Category: CatMissing, Reason: reason, Assertion: assertion, Side: side,
		RecordIDs: []string{r.RecordID}, Exposure: CanonicalAmount(abs), Currency: r.Currency,
		Detail: fmt.Sprintf("reference %q: %s", r.Reference, detail)}
}

func ambiguous(side Side, r PopulationRecord, ref string) FoundException {
	amt, _ := r.ParsedAmount()
	abs := new(big.Rat)
	if amt != nil {
		abs.Abs(amt)
	}
	return FoundException{Category: CatMismatch, Reason: ReasonAmbiguousKey, Assertion: "COMPLETENESS", Side: side,
		RecordIDs: []string{r.RecordID}, Exposure: CanonicalAmount(abs), Currency: r.Currency,
		Detail: fmt.Sprintf("reference %q maps to several non-identical records; explicit allocation required", ref)}
}

func groupByReference(rs []PopulationRecord) map[string][]PopulationRecord {
	m := map[string][]PopulationRecord{}
	for _, r := range rs {
		m[r.Reference] = append(m[r.Reference], r)
	}
	return m
}

func unionKeys(a, b map[string][]PopulationRecord) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameTotals(a, b []CurrencyTotal) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Currency != b[i].Currency || a[i].Total != b[i].Total {
			return false
		}
	}
	return true
}

// unmatchedItem builds the exception for a record with no counterpart. Inside
// the pinned outstanding window before period end it is a RECONCILING ITEM — a
// timing difference with an expected clearing date (deposit in transit,
// outstanding payment) — not a permanent mismatch (§10, §14). Older items stay
// MISSING.
func unmatchedItem(side Side, r PopulationRecord, reason, assertion, detail string, p MatchParams) FoundException {
	if p.OutstandingDays > 0 && !p.PeriodEnd.IsZero() {
		if d, err := time.Parse("2006-01-02", r.Date); err == nil {
			age := int(p.PeriodEnd.Sub(d).Hours() / 24)
			if age >= 0 && age <= p.OutstandingDays {
				amt, _ := r.ParsedAmount()
				abs := new(big.Rat)
				if amt != nil {
					abs.Abs(amt)
				}
				clear := d.AddDate(0, 0, p.OutstandingDays)
				return FoundException{Category: CatTiming, Reason: ReasonOutstandingItem, Assertion: "CUTOFF", Side: side,
					RecordIDs: []string{r.RecordID}, Exposure: CanonicalAmount(abs), Currency: r.Currency,
					Detail: fmt.Sprintf("reference %q is an outstanding reconciling item (%s); expected to clear by %s",
						r.Reference, detail, clear.Format("2006-01-02")),
					ExpectedClearing: &clear}
			}
		}
	}
	return missing(side, r, reason, assertion, detail)
}

// groupMatch reconciles several records sharing one reference as an EXPLICIT
// allocation: the match records exactly which records on each side were
// allocated, so nothing is netted invisibly. It applies only when every record
// is in one currency (no FX without an approved basis), the summed difference is
// within the pinned tolerance. Dates are deliberately NOT compared: the records
// of one allocation (an invoice and its later cash application) legitimately span
// dates, and the period cut-off is enforced by the population's as-of rule.
// Otherwise the whole group becomes one exception naming every record.
func groupMatch(ref string, as, bs []PopulationRecord, tol *big.Rat) (*MatchResult, []FoundException, error) {
	all := append(append([]PopulationRecord{}, as...), bs...)
	ccy := all[0].Currency
	ids := func(rs []PopulationRecord) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.RecordID
		}
		sort.Strings(out)
		return out
	}
	allIDs := append(ids(as), ids(bs)...)

	sumA, sumB := new(big.Rat), new(big.Rat)
	for _, side := range []struct {
		rs  []PopulationRecord
		sum *big.Rat
	}{{as, sumA}, {bs, sumB}} {
		for _, r := range side.rs {
			if r.Currency != ccy {
				return nil, []FoundException{{Category: CatMismatch, Reason: ReasonCurrencyMismatch, Assertion: "ACCURACY",
					Side: SideA, RecordIDs: allIDs, Exposure: "0", Currency: ccy,
					Detail: fmt.Sprintf("reference %q mixes currencies; no allocation is made without an approved FX basis", ref)}}, nil
			}
			amt, err := r.ParsedAmount()
			if err != nil {
				return nil, nil, err
			}
			side.sum.Add(side.sum, amt)
		}
	}
	diff := new(big.Rat).Sub(sumA, sumB)
	absDiff := new(big.Rat).Abs(diff)
	if absDiff.Cmp(tol) > 0 {
		return nil, []FoundException{{Category: CatMismatch, Reason: ReasonGroupMismatch, Assertion: "ACCURACY",
			Side: SideA, RecordIDs: allIDs, Exposure: CanonicalAmount(absDiff), Currency: ccy,
			Detail: fmt.Sprintf("reference %q: side A %d record(s) total %s vs side B %d record(s) total %s (difference %s) exceeds the pinned tolerance %s",
				ref, len(as), CanonicalAmount(sumA), len(bs), CanonicalAmount(sumB), CanonicalAmount(diff), CanonicalAmount(tol))}}, nil
	}
	outcome := OutcomeGroupExact
	if diff.Sign() != 0 {
		outcome = OutcomeGroupTolerance
	}
	aIDs, bIDs := ids(as), ids(bs)
	return &MatchResult{Kind: MatchGroup, Reference: ref, SideARecords: aIDs, SideBRecords: bIDs,
		SideA: aIDs[0], SideB: bIDs[0], Rule: MatchAlgorithmVersion, Outcome: outcome,
		Difference: CanonicalAmount(diff), Currency: ccy}, nil, nil
}
