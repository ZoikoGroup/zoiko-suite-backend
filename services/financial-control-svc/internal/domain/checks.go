package domain

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reason codes for the single-population checks.
const (
	ReasonDuplicateBusinessKey = "DUPLICATE_BUSINESS_KEY"
	ReasonKeyFieldMissing      = "KEY_FIELD_MISSING"
	ReasonSequenceGap          = "SEQUENCE_GAP"
	ReasonDuplicateSequence    = "DUPLICATE_SEQUENCE_NUMBER"
	ReasonUnparseableSequence  = "UNPARSEABLE_SEQUENCE"
	ReasonStatementDayMissing  = "STATEMENT_DAY_MISSING"
	ReasonFindingLimit         = "FINDING_LIMIT_EXCEEDED"

	CatDataQuality = "DATA_QUALITY"
)

// ScanDuplicates finds business events recorded more than once inside ONE
// population (FIN-CTRL-028: supplier invoice duplicate control). The business
// key is composed from logic.KeyFields; the record with the lowest record_id is
// kept and every further record with the same key becomes a DUPLICATE
// exception. A record that lacks a key field is itself an exception — it is
// never skipped, since a silent skip is how a duplicate check gets bypassed.
func ScanDuplicates(recs []PopulationRecord, l RuleLogic) (MatchOutput, error) {
	var out MatchOutput
	unique, idGroups, err := SplitDuplicatesOpt(recs, true)
	if err != nil {
		return out, err
	}
	for _, g := range idGroups {
		out.Exceptions = append(out.Exceptions, duplicateExceptions(SideA, g)...)
	}

	keys := map[string]*PopulationRecord{}
	for i := range unique {
		r := unique[i]
		key, missingField, err := businessKey(r, l.KeyFields)
		if err != nil {
			return out, err
		}
		if missingField != "" {
			out.Exceptions = append(out.Exceptions, dataQuality(r, ReasonKeyFieldMissing,
				fmt.Sprintf("key field %q is empty, so the duplicate check cannot be evaluated for this record", missingField)))
			continue
		}
		first, seen := keys[key]
		if !seen {
			keys[key] = &unique[i]
			continue
		}
		amt, _ := r.ParsedAmount()
		abs := new(big.Rat)
		if amt != nil {
			abs.Abs(amt)
		}
		out.Exceptions = append(out.Exceptions, FoundException{Category: CatDuplicate, Reason: ReasonDuplicateBusinessKey,
			Assertion: "OCCURRENCE", Side: SideA, RecordIDs: []string{r.RecordID}, Exposure: CanonicalAmount(abs),
			Currency: r.Currency, Detail: fmt.Sprintf("same business key (%s) as record %q (reference %q)",
				strings.Join(l.KeyFields, "+"), first.RecordID, first.Reference)})
	}
	sortExceptions(&out)
	return out, nil
}

func businessKey(r PopulationRecord, fields []string) (key, missingField string, err error) {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		var v string
		switch {
		case f == "reference":
			v = r.Reference
		case f == "currency":
			v = r.Currency
		case f == "date":
			v = r.Date
		case f == "amount":
			a, e := r.ParsedAmount()
			if e != nil {
				return "", "", e
			}
			v = CanonicalAmount(a)
		case strings.HasPrefix(f, "attr:"):
			v = strings.TrimSpace(r.Attributes[strings.TrimPrefix(f, "attr:")])
		}
		if strings.TrimSpace(v) == "" {
			return "", f, nil
		}
		parts = append(parts, strings.ToLower(strings.TrimSpace(v)))
	}
	return strings.Join(parts, "\x1f"), "", nil
}

var sequenceRe = regexp.MustCompile(`^(.*?)(\d{1,18})$`)

// ScanSequenceGaps checks numbering completeness (FIN-CTRL-027: customer
// invoice sequence / issue completeness). The number is the trailing integer of
// `reference`; records are grouped by the text before it (INV-, CN-…). Within a
// group every number between the lowest and highest must be present. A gap is
// reported as one exception per contiguous range; a number used twice is a
// duplicate. References with no trailing number are data-quality exceptions.
func ScanSequenceGaps(recs []PopulationRecord, l RuleLogic) (MatchOutput, error) {
	var out MatchOutput
	type num struct {
		n   int64
		rec PopulationRecord
	}
	groups := map[string][]num{}
	width := map[string]int{}
	for _, r := range recs {
		m := sequenceRe.FindStringSubmatch(r.Reference)
		if m == nil {
			out.Exceptions = append(out.Exceptions, dataQuality(r, ReasonUnparseableSequence,
				fmt.Sprintf("reference %q has no trailing sequence number", r.Reference)))
			continue
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return out, fmt.Errorf("%w: sequence number in %q", ErrInvalidArgument, r.Reference)
		}
		groups[m[1]] = append(groups[m[1]], num{n, r})
		if len(m[2]) > width[m[1]] {
			width[m[1]] = len(m[2])
		}
	}

	prefixes := make([]string, 0, len(groups))
	for p := range groups {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)

	findings, suppressed := 0, 0
	add := func(f FoundException) {
		if findings >= l.MaxFindings {
			suppressed++
			return
		}
		findings++
		out.Exceptions = append(out.Exceptions, f)
	}
	for _, p := range prefixes {
		ns := groups[p]
		sort.SliceStable(ns, func(i, j int) bool {
			if ns[i].n != ns[j].n {
				return ns[i].n < ns[j].n
			}
			return ns[i].rec.RecordID < ns[j].rec.RecordID
		})
		ccy := ns[0].rec.Currency
		for i := 1; i < len(ns); i++ {
			prev, cur := ns[i-1], ns[i]
			switch {
			case cur.n == prev.n:
				amt, _ := cur.rec.ParsedAmount()
				abs := new(big.Rat)
				if amt != nil {
					abs.Abs(amt)
				}
				add(FoundException{Category: CatDuplicate, Reason: ReasonDuplicateSequence, Assertion: "COMPLETENESS",
					Side: SideA, RecordIDs: []string{cur.rec.RecordID}, Exposure: CanonicalAmount(abs), Currency: cur.rec.Currency,
					Detail: fmt.Sprintf("sequence number %s%0*d is used by records %q and %q", p, width[p], cur.n, prev.rec.RecordID, cur.rec.RecordID)})
			case cur.n > prev.n+1:
				from, to := prev.n+1, cur.n-1
				add(FoundException{Category: CatMissing, Reason: ReasonSequenceGap, Assertion: "COMPLETENESS", Side: SideA,
					RecordIDs: []string{fmt.Sprintf("gap:%s%0*d-%s%0*d", p, width[p], from, p, width[p], to)},
					Exposure:  "0", Currency: ccy,
					Detail: fmt.Sprintf("%d missing number(s) between %s%0*d and %s%0*d", to-from+1, p, width[p], prev.n, p, width[p], cur.n)})
			}
		}
	}
	if suppressed > 0 {
		out.Exceptions = append(out.Exceptions, limitException(suppressed, "sequence gaps/duplicates", "USD"))
	}
	sortExceptions(&out)
	return out, nil
}

// CheckDateCoverage requires a record for every calendar day of the period, per
// group (FIN-CTRL-032: bank feed / statement completeness, scenario 16). A group
// named in ExpectedGroups must be covered even if it delivered nothing at all,
// so a dead feed cannot pass by producing no records. Coverage is required only
// up to `today` for a period still in progress.
func CheckDateCoverage(recs []PopulationRecord, l RuleLogic, periodID string, today time.Time) (MatchOutput, error) {
	var out MatchOutput
	start, end, ok := PeriodBounds(periodID)
	if !ok {
		return out, fmt.Errorf("%w: DATE_COVERAGE needs a run period_id of the form YYYY-MM", ErrInvalidArgument)
	}
	last := end
	if t := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC); t.Before(last) {
		last = t
	}

	present := map[string]map[string]bool{}
	ccy := "XXX"
	for _, r := range recs {
		g := ""
		if l.GroupAttr != "" {
			g = strings.TrimSpace(r.Attributes[l.GroupAttr])
			if g == "" {
				out.Exceptions = append(out.Exceptions, dataQuality(r, ReasonKeyFieldMissing,
					fmt.Sprintf("attribute %q is empty, so this record cannot be attributed to a feed", l.GroupAttr)))
				continue
			}
		}
		if present[g] == nil {
			present[g] = map[string]bool{}
		}
		present[g][r.Date] = true
		if ccy == "XXX" {
			ccy = r.Currency
		}
	}
	for _, g := range l.ExpectedGroups {
		if present[g] == nil {
			present[g] = map[string]bool{}
		}
	}

	groups := make([]string, 0, len(present))
	for g := range present {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	findings, suppressed := 0, 0
	for _, g := range groups {
		var rangeFrom, rangeTo time.Time
		flush := func() {
			if rangeFrom.IsZero() {
				return
			}
			if findings >= l.MaxFindings {
				suppressed++
			} else {
				findings++
				label := g
				if label == "" {
					label = "feed"
				}
				out.Exceptions = append(out.Exceptions, FoundException{Category: CatMissing, Reason: ReasonStatementDayMissing,
					Assertion: "COMPLETENESS", Side: SideA,
					RecordIDs: []string{fmt.Sprintf("gap:%s:%s..%s", label, rangeFrom.Format("2006-01-02"), rangeTo.Format("2006-01-02"))},
					Exposure:  "0", Currency: ccy,
					Detail: fmt.Sprintf("no record for %s from %s to %s", label, rangeFrom.Format("2006-01-02"), rangeTo.Format("2006-01-02"))})
			}
			rangeFrom, rangeTo = time.Time{}, time.Time{}
		}
		for d := start; !d.After(last); d = d.AddDate(0, 0, 1) {
			if l.SkipWeekends && (d.Weekday() == time.Saturday || d.Weekday() == time.Sunday) {
				continue
			}
			if present[g][d.Format("2006-01-02")] {
				flush()
				continue
			}
			if rangeFrom.IsZero() {
				rangeFrom = d
			}
			rangeTo = d
		}
		flush()
	}
	if suppressed > 0 {
		out.Exceptions = append(out.Exceptions, limitException(suppressed, "missing-day ranges", ccy))
	}
	sortExceptions(&out)
	return out, nil
}

func dataQuality(r PopulationRecord, reason, detail string) FoundException {
	amt, _ := r.ParsedAmount()
	abs := new(big.Rat)
	if amt != nil {
		abs.Abs(amt)
	}
	return FoundException{Category: CatDataQuality, Reason: reason, Assertion: "ACCURACY", Side: SideA,
		RecordIDs: []string{r.RecordID}, Exposure: CanonicalAmount(abs), Currency: r.Currency, Detail: detail}
}

func limitException(suppressed int, what, ccy string) FoundException {
	return FoundException{Category: CatDataQuality, Reason: ReasonFindingLimit, Assertion: "COMPLETENESS", Side: SideA,
		RecordIDs: []string{"limit"}, Exposure: "0", Currency: ccy,
		Detail: fmt.Sprintf("%d further %s were suppressed by max_findings; the control has more findings than it reports", suppressed, what)}
}

func sortExceptions(o *MatchOutput) {
	sort.SliceStable(o.Exceptions, func(i, j int) bool {
		x, y := o.Exceptions[i], o.Exceptions[j]
		if x.Reason != y.Reason {
			return x.Reason < y.Reason
		}
		return x.RecordIDs[0] < y.RecordIDs[0]
	})
}

// Reason codes for ARITHMETIC.
const (
	ReasonEquationViolation = "EQUATION_VIOLATION"
	ReasonOperandInvalid    = "OPERAND_MISSING_OR_INVALID"
)

// CheckArithmetic verifies that every equation holds on every record, in exact
// decimal arithmetic, within the pinned absolute tolerance (§15 gross-to-net:
// "Gross earnings − employee deductions/taxes = net pay at worker level").
//
// It is deliberately record-level: a payroll whose run totals happen to tie can
// still contain individual slips that do not foot. A record with a missing or
// non-decimal operand is a DATA_QUALITY exception — never skipped, since a skipped
// record is a record the control silently did not test.
func CheckArithmetic(recs []PopulationRecord, l RuleLogic, tol *big.Rat) (MatchOutput, error) {
	var out MatchOutput
	if tol == nil {
		tol = new(big.Rat)
	}
	findings, suppressed := 0, 0
	add := func(f FoundException) {
		if findings >= l.MaxFindings {
			suppressed++
			return
		}
		findings++
		out.Exceptions = append(out.Exceptions, f)
	}
	sorted := make([]PopulationRecord, len(recs))
	copy(sorted, recs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })

	operand := func(r PopulationRecord, name string) (*big.Rat, error) {
		var raw string
		if name == "amount" {
			raw = r.Amount
		} else {
			v, ok := r.Attributes[strings.TrimPrefix(name, "attr:")]
			if !ok {
				return nil, fmt.Errorf("%s is absent", name)
			}
			raw = v
		}
		x, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
		if !ok {
			return nil, fmt.Errorf("%s=%q is not a decimal", name, raw)
		}
		return x, nil
	}
	sum := func(r PopulationRecord, names []string) (*big.Rat, error) {
		total := new(big.Rat)
		for _, n := range names {
			v, err := operand(r, n)
			if err != nil {
				return nil, err
			}
			total.Add(total, v)
		}
		return total, nil
	}

	for _, r := range sorted {
		for _, eq := range l.Equations {
			plus, err := sum(r, eq.Plus)
			var minus, want *big.Rat
			if err == nil {
				minus, err = sum(r, eq.Minus)
			}
			if err == nil {
				want, err = operand(r, eq.Equals)
			}
			if err != nil {
				fe := dataQuality(r, ReasonOperandInvalid, fmt.Sprintf("equation %q cannot be evaluated: %v", eq.Name, err))
				add(fe)
				continue
			}
			have := new(big.Rat).Sub(plus, minus)
			diff := new(big.Rat).Sub(have, want)
			abs := new(big.Rat).Abs(diff)
			if abs.Cmp(tol) > 0 {
				add(FoundException{Category: CatMismatch, Reason: ReasonEquationViolation, Assertion: "ACCURACY", Side: SideA,
					RecordIDs: []string{r.RecordID}, Exposure: CanonicalAmount(abs), Currency: r.Currency,
					Detail: fmt.Sprintf("%s: %s − %s = %s but %s is %s (difference %s, tolerance %s)", eq.Name,
						strings.Join(eq.Plus, " + "), joinOrZero(eq.Minus), CanonicalAmount(have), eq.Equals,
						CanonicalAmount(want), CanonicalAmount(diff), CanonicalAmount(tol))})
			}
		}
	}
	if suppressed > 0 {
		out.Exceptions = append(out.Exceptions, limitException(suppressed, "equation violations", "XXX"))
	}
	sortExceptionsByRecord(&out)
	return out, nil
}

func joinOrZero(names []string) string {
	if len(names) == 0 {
		return "0"
	}
	return strings.Join(names, " − ")
}

func sortExceptionsByRecord(o *MatchOutput) {
	sort.SliceStable(o.Exceptions, func(i, j int) bool {
		x, y := o.Exceptions[i], o.Exceptions[j]
		if x.RecordIDs[0] != y.RecordIDs[0] {
			return x.RecordIDs[0] < y.RecordIDs[0]
		}
		return x.Reason < y.Reason
	})
}
