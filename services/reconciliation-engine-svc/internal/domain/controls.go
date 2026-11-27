package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source-declared controls (ZS-SVC-Z-001 DQC-03: hash total, sequence total,
// cross-period roll-forward, and the PARTIAL result).
//
// A source MAY declare, next to its row count and per-currency totals, further
// facts about the population it is delivering. The engine does not trust them
// and does not skip them: each declared control is re-performed against the
// records actually received, and a control that does not hold fails the
// population closed (the run is INDETERMINATE), exactly as a wrong row count
// does. A control the source did NOT declare is recorded as NOT_DECLARED. It is
// never recorded as passed: an absent control proves nothing.
//
// The wire shape is an additive extension of `declared_totals` in
// docs/architecture/control-population-contract.md; a source that sends none of
// it behaves exactly as before.

// ErrDeclaredControl is returned when a declared control does not hold against
// the records received, or is itself malformed or in a format this engine does
// not know. The source client maps it to a population that is not reproducible.
var ErrDeclaredControl = errors.New("source declared control does not hold")

// SourceHashSerialization names how a SOURCE computes the content hash it
// declares. It is deliberately independent of the engine's own snapshot hash
// ("rpop-v1", which is over minor units at the run's scale): a source does not
// know the run's scale, so this one is over the wire fields exactly as the
// contract defines them. See SourceContentHash for the exact bytes.
const (
	SourceHashAlgorithm     = "sha256"
	SourceHashSerialization = "cpc-v1"
)

// Completeness scopes a source may declare for the population it delivers.
const (
	ScopeFull    = "FULL"
	ScopePartial = "PARTIAL"
)

// Control names and outcome statuses recorded with a snapshot.
const (
	ControlHash         = "hash"
	ControlSequence     = "sequence"
	ControlRollForward  = "roll_forward"
	ControlCompleteness = "completeness"

	OutcomeVerified     = "VERIFIED"
	OutcomeNotDeclared  = "NOT_DECLARED"
	OutcomeDeclaredFull = "DECLARED_FULL"
	OutcomePartial      = "DECLARED_PARTIAL"
)

type DeclaredHash struct {
	Algorithm     string `json:"algorithm"`
	Serialization string `json:"serialization"`
	Value         string `json:"value"`
}

// DeclaredSequence: every record carries Attribute, an integer, and together
// they are exactly First..Last, each once. A gap or a duplicate is a missing or
// double-counted record.
type DeclaredSequence struct {
	Attribute string `json:"attribute"`
	First     int64  `json:"first"`
	Last      int64  `json:"last"`
}

// DeclaredRollForward: for each currency, Opening + the sum of the records =
// Closing. PeriodStart/PeriodEnd (YYYY-MM-DD) say which period the movement is,
// so the engine can also check that this opening is the previous period's
// closing (cross-period continuity).
type DeclaredRollForward struct {
	Opening     map[string]string `json:"opening"`
	Closing     map[string]string `json:"closing"`
	PeriodStart string            `json:"period_start,omitempty"`
	PeriodEnd   string            `json:"period_end,omitempty"`
}

// DeclaredCompleteness: FULL (the default when absent) says the records are the
// whole in-scope population. PARTIAL says they are a defined subset, and the
// source must say which (Description); a PARTIAL population can be reconciled
// but never certified as the full population (INV-26).
type DeclaredCompleteness struct {
	Scope       string `json:"scope"`
	Description string `json:"description,omitempty"`
}

// DeclaredControls is every optional control a source declared.
type DeclaredControls struct {
	Hash         *DeclaredHash         `json:"hash,omitempty"`
	Sequence     *DeclaredSequence     `json:"sequence,omitempty"`
	RollForward  *DeclaredRollForward  `json:"roll_forward,omitempty"`
	Completeness *DeclaredCompleteness `json:"completeness,omitempty"`
}

// IsZero reports whether the source declared none of the optional controls.
func (c DeclaredControls) IsZero() bool {
	return c.Hash == nil && c.Sequence == nil && c.RollForward == nil && c.Completeness == nil
}

// Partial reports whether the source declared the population a subset.
func (c DeclaredControls) Partial() bool {
	return c.Completeness != nil && c.Completeness.Scope == ScopePartial
}

// ControlOutcome is what the engine concluded about one control.
type ControlOutcome struct {
	Control string `json:"control"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// canonicalDecimal renders a decimal string with no insignificant zeros, so "6",
// "6.0" and "6.00" hash alike.
func canonicalDecimal(s string) (string, error) {
	v, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return "", fmt.Errorf("%w: %q is not a decimal amount", ErrInvalidArgument, s)
	}
	for k := 0; k <= 30; k++ {
		scaled := new(big.Rat).Mul(v, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(k)), nil)))
		if scaled.IsInt() {
			return v.FloatString(k), nil
		}
	}
	return "", fmt.Errorf("%w: %q has more than 30 decimal places", ErrInvalidArgument, s)
}

// SourceContentHash is serialization "cpc-v1": records sorted by record_id
// (byte order); one line each, `%q|%q|<amount>|<currency>|<date>` (record_id,
// reference, amount with no insignificant zeros, currency, date), then for each
// attribute in key order `|%q=%q`, then a newline; sha256, lower-case hex. A
// source computes this over every record of the population (not the page).
// Pinned by a golden vector: a change to these bytes needs a new serialization
// name, never an edit.
func SourceContentHash(records []PopulationRecord) (string, error) {
	sorted := make([]PopulationRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })
	h := sha256.New()
	for _, r := range sorted {
		amt, err := canonicalDecimal(r.Amount)
		if err != nil {
			return "", err
		}
		keys := make([]string, 0, len(r.Attributes))
		for k := range r.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(h, "%q|%q|%s|%s|%s", r.RecordID, r.Reference, amt, r.Currency, r.Date)
		for _, k := range keys {
			fmt.Fprintf(h, "|%q=%q", k, r.Attributes[k])
		}
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyDeclaredControls re-performs every control the source declared against
// the records received. It returns an outcome per control, or an error wrapping
// ErrDeclaredControl for the first control that does not hold. Nothing is ever
// coerced: an unknown hash format, a missing sequence attribute or a currency
// with no declared opening is a failure, not a skip.
func VerifyDeclaredControls(c DeclaredControls, records []PopulationRecord) ([]ControlOutcome, error) {
	out := make([]ControlOutcome, 0, 4)

	if c.Hash == nil {
		out = append(out, ControlOutcome{Control: ControlHash, Status: OutcomeNotDeclared})
	} else {
		o, err := verifyHash(*c.Hash, records)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if c.Sequence == nil {
		out = append(out, ControlOutcome{Control: ControlSequence, Status: OutcomeNotDeclared})
	} else {
		o, err := verifySequence(*c.Sequence, records)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if c.RollForward == nil {
		out = append(out, ControlOutcome{Control: ControlRollForward, Status: OutcomeNotDeclared})
	} else {
		o, err := verifyRollForward(*c.RollForward, records)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	switch {
	case c.Completeness == nil:
		out = append(out, ControlOutcome{Control: ControlCompleteness, Status: OutcomeNotDeclared,
			Detail: "treated as the whole in-scope population, as the contract has always said"})
	case c.Completeness.Scope == ScopeFull:
		out = append(out, ControlOutcome{Control: ControlCompleteness, Status: OutcomeDeclaredFull})
	case c.Completeness.Scope == ScopePartial:
		if strings.TrimSpace(c.Completeness.Description) == "" {
			return nil, fmt.Errorf("%w: a PARTIAL population must say which subset it is", ErrDeclaredControl)
		}
		out = append(out, ControlOutcome{Control: ControlCompleteness, Status: OutcomePartial, Detail: c.Completeness.Description})
	default:
		return nil, fmt.Errorf("%w: unknown completeness scope %q", ErrDeclaredControl, c.Completeness.Scope)
	}
	return out, nil
}

func verifyHash(d DeclaredHash, records []PopulationRecord) (ControlOutcome, error) {
	if d.Algorithm != SourceHashAlgorithm || d.Serialization != SourceHashSerialization {
		// INV-15 / INV-24: a format this engine cannot re-derive is reported, never guessed at.
		return ControlOutcome{}, fmt.Errorf("%w: hash format %q/%q is not one this engine can re-derive", ErrDeclaredControl, d.Algorithm, d.Serialization)
	}
	got, err := SourceContentHash(records)
	if err != nil {
		return ControlOutcome{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(d.Value), got) {
		return ControlOutcome{}, fmt.Errorf("%w: hash total is %s, source declared %s", ErrDeclaredControl, got, d.Value)
	}
	return ControlOutcome{Control: ControlHash, Status: OutcomeVerified, Detail: d.Algorithm + "/" + d.Serialization}, nil
}

func verifySequence(d DeclaredSequence, records []PopulationRecord) (ControlOutcome, error) {
	if strings.TrimSpace(d.Attribute) == "" {
		return ControlOutcome{}, fmt.Errorf("%w: a declared sequence must name the attribute that carries it", ErrDeclaredControl)
	}
	if d.Last < d.First {
		return ControlOutcome{}, fmt.Errorf("%w: sequence runs from %d to %d", ErrDeclaredControl, d.First, d.Last)
	}
	// Nothing is allocated by the declared span: the scan for missing numbers stops at ten
	// findings, so an absurd range is reported as missing numbers, never expanded.
	seen := make(map[int64]string, len(records))
	var problems []string
	add := func(format string, a ...any) {
		if len(problems) < 10 {
			problems = append(problems, fmt.Sprintf(format, a...))
		}
	}
	for _, r := range records {
		raw, ok := r.Attributes[d.Attribute]
		if !ok {
			add("record %q has no %q", r.RecordID, d.Attribute)
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			add("record %q has non-integer %s %q", r.RecordID, d.Attribute, raw)
			continue
		}
		if n < d.First || n > d.Last {
			add("record %q has %s %d outside %d..%d", r.RecordID, d.Attribute, n, d.First, d.Last)
			continue
		}
		if prev, dup := seen[n]; dup {
			add("%s %d appears on records %q and %q", d.Attribute, n, prev, r.RecordID)
			continue
		}
		seen[n] = r.RecordID
	}
	for n := d.First; n <= d.Last && len(problems) < 10; n++ {
		if _, ok := seen[n]; !ok {
			add("%s %d is missing", d.Attribute, n)
		}
	}
	if len(problems) > 0 {
		return ControlOutcome{}, fmt.Errorf("%w: sequence %q %d..%d is not continuous: %s", ErrDeclaredControl, d.Attribute, d.First, d.Last, strings.Join(problems, "; "))
	}
	return ControlOutcome{Control: ControlSequence, Status: OutcomeVerified,
		Detail: fmt.Sprintf("%s %d..%d, %d records, no gap, no duplicate", d.Attribute, d.First, d.Last, len(records))}, nil
}

func verifyRollForward(d DeclaredRollForward, records []PopulationRecord) (ControlOutcome, error) {
	if (d.PeriodStart == "") != (d.PeriodEnd == "") {
		return ControlOutcome{}, fmt.Errorf("%w: roll-forward period_start and period_end go together", ErrDeclaredControl)
	}
	if d.PeriodStart != "" {
		s, err1 := time.Parse("2006-01-02", d.PeriodStart)
		e, err2 := time.Parse("2006-01-02", d.PeriodEnd)
		if err1 != nil || err2 != nil || e.Before(s) {
			return ControlOutcome{}, fmt.Errorf("%w: roll-forward period %q..%q is not a valid date range", ErrDeclaredControl, d.PeriodStart, d.PeriodEnd)
		}
	}
	movement, err := DecimalTotals(records)
	if err != nil {
		return ControlOutcome{}, err
	}
	ccys := map[string]bool{}
	for c := range movement {
		ccys[c] = true
	}
	for c := range d.Opening {
		ccys[c] = true
	}
	for c := range d.Closing {
		ccys[c] = true
	}
	names := make([]string, 0, len(ccys))
	for c := range ccys {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		o, okO := parseDeclared(d.Opening[c])
		cl, okC := parseDeclared(d.Closing[c])
		if !okO || !okC {
			// A missing side is never read as zero: that is how a roll-forward is made to tie.
			return ControlOutcome{}, fmt.Errorf("%w: roll-forward has no opening and closing balance for %s", ErrDeclaredControl, c)
		}
		mv := movement[c]
		if mv == nil {
			mv = new(big.Rat)
		}
		if got := new(big.Rat).Add(o, mv); got.Cmp(cl) != 0 {
			return ControlOutcome{}, fmt.Errorf("%w: %s opening %s + movement %s = %s, source declared closing %s",
				ErrDeclaredControl, c, o.FloatString(6), mv.FloatString(6), got.FloatString(6), cl.FloatString(6))
		}
	}
	return ControlOutcome{Control: ControlRollForward, Status: OutcomeVerified,
		Detail: fmt.Sprintf("opening + movement = closing in %d currenc(ies)", len(names))}, nil
}

func parseDeclared(s string) (*big.Rat, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, false
	}
	return new(big.Rat).SetString(strings.TrimSpace(s))
}

// OpeningContinuity compares this population's declared opening with the
// closing the previous period declared (cross-period roll-forward): they must
// be the same number in every currency either names. A currency missing on one
// side is a break, never a zero.
func OpeningContinuity(opening, priorClosing map[string]string) error {
	ccys := map[string]bool{}
	for c := range opening {
		ccys[c] = true
	}
	for c := range priorClosing {
		ccys[c] = true
	}
	names := make([]string, 0, len(ccys))
	for c := range ccys {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		o, okO := parseDeclared(opening[c])
		p, okP := parseDeclared(priorClosing[c])
		if !okO || !okP || o.Cmp(p) != 0 {
			return fmt.Errorf("%w: %s opens at %q but the previous period closed at %q", ErrDeclaredControl, c, opening[c], priorClosing[c])
		}
	}
	return nil
}

// Cross-period continuity is checked by the service, not the source: it needs the previous
// period's certified closing, which only this engine holds.
const (
	ControlContinuity    = "continuity"
	OutcomeNoPriorPeriod = "NO_PRIOR_PERIOD"
)

// PriorClosing is the closing balance an earlier, certified, not-revoked run declared for the
// same population, with the last day of the period it covers.
type PriorClosing struct {
	RunID     string            `json:"run_id"`
	PeriodEnd string            `json:"period_end"`
	Closing   map[string]string `json:"closing"`
}

// CheckContinuity decides the continuity outcome of one population's declared roll-forward
// against the previous certified period (nil when there is none). With no earlier certified
// period the opening is NOT independently verified, and the outcome says so rather than passing.
// An earlier period that is not the immediately preceding one means a period is missing between
// them, which the control cannot bridge, so that is a failure too.
func CheckContinuity(rf DeclaredRollForward, prior *PriorClosing) (ControlOutcome, error) {
	if rf.PeriodStart == "" {
		return ControlOutcome{Control: ControlContinuity, Status: OutcomeNotDeclared,
			Detail: "the roll-forward names no period, so it cannot be tied to a previous one"}, nil
	}
	if prior == nil {
		return ControlOutcome{Control: ControlContinuity, Status: OutcomeNoPriorPeriod,
			Detail: "no earlier certified period declares a closing balance; the opening is not independently verified"}, nil
	}
	start, err := time.Parse("2006-01-02", rf.PeriodStart)
	if err != nil {
		return ControlOutcome{}, fmt.Errorf("%w: roll-forward period_start %q", ErrDeclaredControl, rf.PeriodStart)
	}
	end, err := time.Parse("2006-01-02", prior.PeriodEnd)
	if err != nil {
		return ControlOutcome{}, fmt.Errorf("%w: the previous period's end %q", ErrDeclaredControl, prior.PeriodEnd)
	}
	if !end.AddDate(0, 0, 1).Equal(start) {
		return ControlOutcome{}, fmt.Errorf("%w: this period starts %s but the latest certified period (run %s) ended %s: the periods between are not certified",
			ErrDeclaredControl, rf.PeriodStart, prior.RunID, prior.PeriodEnd)
	}
	if err := OpeningContinuity(rf.Opening, prior.Closing); err != nil {
		return ControlOutcome{}, fmt.Errorf("%w (run %s)", err, prior.RunID)
	}
	return ControlOutcome{Control: ControlContinuity, Status: OutcomeVerified,
		Detail: fmt.Sprintf("opening equals the closing of certified run %s (period ended %s)", prior.RunID, prior.PeriodEnd)}, nil
}
