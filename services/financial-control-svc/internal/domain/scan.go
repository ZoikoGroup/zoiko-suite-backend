package domain

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// KindExceptionScan: the population IS the finding set. It is used for controls
// whose source system already answers "which records violate the policy" (a
// direct posting to a restricted control account, an accounting event that never
// became a journal). Every fetched record is an exception unless Conditions are
// given, in which case only records matching at least one condition are.
//
// A scan over an EMPTY population passes, so the source endpoint must be a
// complete population for the period/scope — completeness is proven by the
// population's watermark and declared totals, exactly as for every other check.
const KindExceptionScan = "EXCEPTION_SCAN"

// Condition ops.
const (
	OpNonEmpty = "nonempty" // attribute present and not blank
	OpEmpty    = "empty"    // attribute absent or blank
	OpEq       = "eq"
	OpNe       = "ne" // absent counts as different
)

// ScanCondition marks a record as a finding when it holds.
type ScanCondition struct {
	Attr   string `json:"attr"`
	Op     string `json:"op"`
	Value  string `json:"value"`
	Detail string `json:"detail"`
}

var scanCategories = map[string]bool{
	"CLASSIFICATION": true, "AUTHORIZATION": true, "LATE_DATA": true, "DATA_QUALITY": true,
	"MISSING": true, "MISMATCH": true, "TIMING": true, "EXTERNAL_STATUS": true,
}

var (
	scanReasonRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,47}$`)
	attrNameRe   = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
)

func (l RuleLogic) validateScan() error {
	if !scanCategories[l.FindingCategory] {
		return fmt.Errorf("EXCEPTION_SCAN needs finding_category, one of CLASSIFICATION, AUTHORIZATION, LATE_DATA, DATA_QUALITY, EXTERNAL_STATUS, MISSING, MISMATCH, TIMING")
	}
	if !scanReasonRe.MatchString(l.FindingReason) {
		return fmt.Errorf("EXCEPTION_SCAN needs finding_reason in UPPER_SNAKE_CASE")
	}
	if strings.TrimSpace(l.FindingAssertion) == "" {
		return fmt.Errorf("EXCEPTION_SCAN needs finding_assertion")
	}
	for _, c := range l.Conditions {
		if !attrNameRe.MatchString(c.Attr) {
			return fmt.Errorf("condition attr must be a simple attribute name")
		}
		switch c.Op {
		case OpNonEmpty, OpEmpty:
		case OpEq, OpNe:
			if c.Value == "" {
				return fmt.Errorf("condition %s on %s needs a value", c.Op, c.Attr)
			}
		default:
			return fmt.Errorf("unknown condition op %q", c.Op)
		}
	}
	return nil
}

func (c ScanCondition) holds(r PopulationRecord) bool {
	v := strings.TrimSpace(r.Attributes[c.Attr])
	switch c.Op {
	case OpNonEmpty:
		return v != ""
	case OpEmpty:
		return v == ""
	case OpEq:
		return v == c.Value
	case OpNe:
		return v != c.Value
	}
	return false
}

// ScanExceptions raises one exception per matching record. Exposure is the
// record's absolute amount; a record with a non-decimal amount is still a
// finding (with zero exposure) because the violation does not depend on it.
func ScanExceptions(recs []PopulationRecord, l RuleLogic) (MatchOutput, error) {
	var out MatchOutput
	sorted := make([]PopulationRecord, len(recs))
	copy(sorted, recs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })

	findings, suppressed := 0, 0
	for _, r := range sorted {
		detail := ""
		hit := len(l.Conditions) == 0
		if hit {
			detail = "record is in the violation population"
		}
		for _, c := range l.Conditions {
			if c.holds(r) {
				hit = true
				detail = c.Detail
				if detail == "" {
					detail = fmt.Sprintf("%s %s %s", c.Attr, c.Op, c.Value)
				}
				break
			}
		}
		if !hit {
			continue
		}
		if findings >= l.MaxFindings {
			suppressed++
			continue
		}
		findings++
		exp := new(big.Rat)
		if a, err := r.ParsedAmount(); err == nil && a != nil {
			exp.Abs(a)
		}
		out.Exceptions = append(out.Exceptions, FoundException{Category: l.FindingCategory, Reason: l.FindingReason,
			Assertion: l.FindingAssertion, Side: SideA, RecordIDs: []string{r.RecordID},
			Exposure: CanonicalAmount(exp), Currency: r.Currency, Detail: detail})
	}
	if suppressed > 0 {
		out.Exceptions = append(out.Exceptions, limitException(suppressed, "policy violations", "XXX"))
	}
	sortExceptionsByRecord(&out)
	return out, nil
}
