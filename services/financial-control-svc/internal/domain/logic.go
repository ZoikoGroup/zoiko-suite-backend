package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Check kinds a control's rule logic can select.
const (
	KindMatch         = "MATCH"          // two-sided record-level reconciliation (side A vs side B)
	KindDuplicateScan = "DUPLICATE_SCAN" // one population: same business event recorded twice
	KindSequenceGap   = "SEQUENCE_GAP"   // one population: numbering gaps (invoice completeness)
	KindDateCoverage  = "DATE_COVERAGE"  // one population: missing calendar days (statement completeness)
	KindArithmetic    = "ARITHMETIC"     // one population: an equation must hold on every record (gross-to-net)
)

// RuleLogic is the executable content of a control_rule_versions.logic
// document. It is read from the run's PINNED rule version, so an approved
// version's behaviour can never change under a run (Invariant 4).
type RuleLogic struct {
	Kind string `json:"kind"`

	// MATCH
	//
	// AllowGroups lets several records that share one reference be reconciled as an
	// explicit allocation (split payments, consolidated receipts, invoice + credit
	// note + cash application). Without it such a reference is an AMBIGUOUS_MATCH_KEY
	// exception: the engine never guesses an allocation.
	AllowGroups bool `json:"allow_groups"`
	// SkipContentDuplicates disables "identical content = duplicate" detection. Group
	// controls turn it off because identical legitimate postings exist (two equal
	// partial payments on one day); a real double posting then shows up as a group
	// difference instead. Repeated record_ids are ALWAYS duplicates.
	SkipContentDuplicates bool `json:"skip_content_duplicates"`
	// OutstandingDays: an unmatched record dated within this many days of the period
	// end is a timing/reconciling item (deposit in transit, outstanding payment) with
	// an expected clearing date, not a permanent mismatch (§10). 0 disables it.
	OutstandingDays int `json:"outstanding_days"`

	// DUPLICATE_SCAN: fields forming the business key. Any of reference, amount,
	// currency, date, or attr:<name>.
	KeyFields []string `json:"key_fields"`

	// SEQUENCE_GAP: numbering is the trailing integer of `reference` (e.g. INV-000123).
	// SEQUENCE_GAP / DATE_COVERAGE: MaxFindings bounds exceptions raised for one run
	// (a final GAP_LIMIT_EXCEEDED exception states how many were suppressed).
	MaxFindings int `json:"max_findings"`

	// ARITHMETIC: every record must satisfy every equation, within the PINNED absolute
	// tolerance. An operand that is missing or not a decimal is an exception, never a skip.
	Equations []Equation `json:"equations"`

	// Exclusions are the ONLY way a fetched record leaves a population (§9). Each names
	// its reason and the authority that approved it, and its count/value impact is
	// recorded on the frozen snapshot and in the evidence — never silent.
	Exclusions []ExclusionRule `json:"exclusions"`

	// DATE_COVERAGE: every calendar day of the period must be present for each group.
	GroupAttr      string   `json:"group_attr"`      // attribute grouping records (bank_account_id)
	ExpectedGroups []string `json:"expected_groups"` // groups that MUST appear even if they have no records
	SkipWeekends   bool     `json:"skip_weekends"`

	// EXCEPTION_SCAN: see KindExceptionScan.
	Conditions       []ScanCondition `json:"conditions"`
	FindingCategory  string          `json:"finding_category"`
	FindingReason    string          `json:"finding_reason"`
	FindingAssertion string          `json:"finding_assertion"`
}

const defaultMaxFindings = 1000

var attrFieldRe = regexp.MustCompile(`^attr:[a-z0-9_]{1,32}$`)

// ParseRuleLogic parses and validates a rule logic document. An empty or
// kind-less document is a plain MATCH with no groups (Wave 1 behaviour).
func ParseRuleLogic(raw json.RawMessage) (RuleLogic, error) {
	var l RuleLogic
	if len(raw) > 0 && string(raw) != "null" {
		// Strict: an unrecognised key (a misspelt allow_group) must be an error, because
		// silently ignoring it would change what the control does.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return l, fmt.Errorf("%w: rule logic: %v", ErrInvalidArgument, err)
		}
	}
	if l.Kind == "" {
		l.Kind = KindMatch
	}
	l.Kind = strings.ToUpper(l.Kind)
	bad := func(m string) (RuleLogic, error) { return l, fmt.Errorf("%w: rule logic: %s", ErrInvalidArgument, m) }
	switch l.Kind {
	case KindMatch:
		if l.OutstandingDays < 0 || l.OutstandingDays > 366 {
			return bad("outstanding_days must be between 0 and 366")
		}
	case KindDuplicateScan:
		if len(l.KeyFields) == 0 {
			return bad("DUPLICATE_SCAN needs key_fields")
		}
		for _, f := range l.KeyFields {
			switch f {
			case "reference", "amount", "currency", "date":
			default:
				if !attrFieldRe.MatchString(f) {
					return bad(fmt.Sprintf("unknown key field %q", f))
				}
			}
		}
	case KindExceptionScan:
		if err := l.validateScan(); err != nil {
			return bad(err.Error())
		}
	case KindSequenceGap:
	case KindArithmetic:
		if len(l.Equations) == 0 {
			return bad("ARITHMETIC needs at least one equation")
		}
		for _, e := range l.Equations {
			if err := e.validate(); err != nil {
				return bad(err.Error())
			}
		}
	case KindDateCoverage:
		if l.GroupAttr != "" && !regexp.MustCompile(`^[a-z0-9_]{1,32}$`).MatchString(l.GroupAttr) {
			return bad("group_attr must be a simple attribute name")
		}
		if len(l.ExpectedGroups) > 0 && l.GroupAttr == "" {
			return bad("expected_groups requires group_attr")
		}
	default:
		return bad(fmt.Sprintf("unknown kind %q", l.Kind))
	}
	for _, x := range l.Exclusions {
		if err := x.validate(); err != nil {
			return bad(err.Error())
		}
	}
	if l.MaxFindings < 0 {
		return bad("max_findings must be >= 0")
	}
	if l.MaxFindings == 0 {
		l.MaxFindings = defaultMaxFindings
	}
	return l, nil
}

// TwoSided reports whether the check compares two populations.
func (l RuleLogic) TwoSided() bool { return l.Kind == KindMatch }

// PeriodBounds parses a YYYY-MM period id into its first and last day.
func PeriodBounds(periodID string) (start, end time.Time, ok bool) {
	t, err := time.Parse("2006-01", periodID)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return t, t.AddDate(0, 1, -1), true
}

// ExclusionRule excludes records whose attribute holds one of Values from a
// population, on one side ("A", "B") or both (""). Reason and Authority are
// mandatory: an exclusion nobody owns is a way to make a control pass.
type ExclusionRule struct {
	Side      string   `json:"side"`
	Attr      string   `json:"attr"`
	Values    []string `json:"values"`
	Reason    string   `json:"reason"`
	Authority string   `json:"authority"`
}

func (e ExclusionRule) validate() error {
	if e.Side != "" && e.Side != "A" && e.Side != "B" {
		return fmt.Errorf("exclusion side must be A, B or empty")
	}
	if !regexp.MustCompile(`^[a-z0-9_]{1,32}$`).MatchString(e.Attr) {
		return fmt.Errorf("exclusion attr must be a simple attribute name")
	}
	if len(e.Values) == 0 {
		return fmt.Errorf("exclusion needs at least one value")
	}
	if strings.TrimSpace(e.Reason) == "" || strings.TrimSpace(e.Authority) == "" {
		return fmt.Errorf("every exclusion needs a reason and the approving authority")
	}
	return nil
}

// ApplyExclusions removes the records the rules exclude from one side and
// reports each rule's impact per currency, with a digest of the excluded record
// ids so the exclusion can be reproduced and audited.
func ApplyExclusions(recs []PopulationRecord, rules []ExclusionRule, side Side) ([]PopulationRecord, []Exclusion, error) {
	if len(rules) == 0 {
		return recs, []Exclusion{}, nil
	}
	type agg struct {
		ids   []string
		count int
		sum   *big.Rat
	}
	perRule := make([]map[string]*agg, len(rules))
	for i := range perRule {
		perRule[i] = map[string]*agg{}
	}
	kept := make([]PopulationRecord, 0, len(recs))
	for _, r := range recs {
		excluded := false
		for i, rule := range rules {
			if rule.Side != "" && rule.Side != string(side) {
				continue
			}
			v, ok := r.Attributes[rule.Attr]
			if !ok {
				continue
			}
			for _, x := range rule.Values {
				if v == x {
					amt, err := r.ParsedAmount()
					if err != nil {
						return nil, nil, err
					}
					a := perRule[i][r.Currency]
					if a == nil {
						a = &agg{sum: new(big.Rat)}
						perRule[i][r.Currency] = a
					}
					a.ids = append(a.ids, r.RecordID)
					a.count++
					a.sum.Add(a.sum, amt)
					excluded = true
					break
				}
			}
			if excluded {
				break // first matching rule owns the exclusion
			}
		}
		if !excluded {
			kept = append(kept, r)
		}
	}
	var out []Exclusion
	for i, rule := range rules {
		ccys := make([]string, 0, len(perRule[i]))
		for c := range perRule[i] {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		for _, c := range ccys {
			a := perRule[i][c]
			sort.Strings(a.ids)
			sum := sha256.Sum256([]byte(strings.Join(a.ids, "\n")))
			out = append(out, Exclusion{
				Reason:    fmt.Sprintf("%s (%s in %s)", rule.Reason, rule.Attr, strings.Join(rule.Values, ",")),
				Authority: rule.Authority, Count: a.count, Value: CanonicalAmount(a.sum), Currency: c,
				Digest: "sha256:" + hex.EncodeToString(sum[:]),
			})
		}
	}
	if out == nil {
		out = []Exclusion{}
	}
	return kept, out, nil
}

// Equation states that sum(Plus) − sum(Minus) = Equals on every record.
// Operands are "amount" or "attr:<name>", each an exact decimal.
type Equation struct {
	Name   string   `json:"name"`
	Plus   []string `json:"plus"`
	Minus  []string `json:"minus"`
	Equals string   `json:"equals"`
}

var operandRe = regexp.MustCompile(`^(amount|attr:[a-z0-9_]{1,32})$`)

func (e Equation) validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("every equation needs a name")
	}
	if len(e.Plus) == 0 || e.Equals == "" {
		return fmt.Errorf("equation %q needs plus operands and an equals operand", e.Name)
	}
	for _, o := range append(append([]string{e.Equals}, e.Plus...), e.Minus...) {
		if !operandRe.MatchString(o) {
			return fmt.Errorf("equation %q: operand %q must be \"amount\" or \"attr:<name>\"", e.Name, o)
		}
	}
	return nil
}
