// Package abac evaluates declared attribute conditions against the attributes
// a calling service supplies with an authorization request.
//
// This package is the "ABAC decision logic" the service spec assigns
// authorization-svc, and it is deliberately ONLY that. It contains no rule: no
// threshold, no attribute name, no action code. Every rule is a row in
// abac_rules, authored by somebody who knows the business, and with no rows
// this package's Evaluate is a no-op that returns "not denied".
//
// The distinction matters because it is the one progress.md's v1 note
// collapsed. "No attribute-condition rules exist anywhere in the architecture
// docs to encode" was true and still is; it argued against inventing RULES,
// and was then read as an argument against building the ENGINE. sod_rules is
// the same shape and nobody calls it invented business logic: a table of
// declarations, a query, and no opinion in the code about which declarations
// exist.
//
// ── DENY-ONLY ────────────────────────────────────────────────────────────────
//
// Evaluate can only ever turn an already-GRANTED decision into a denial. It
// cannot grant. See domain.ABACRule for why that is structural rather than
// stylistic.
package abac

import (
	"strconv"
	"strings"

	"zoiko.io/authorization-svc/internal/domain"
)

// Denial is a rule that refused the request. The zero value means no rule
// refused it.
type Denial struct {
	// RuleCode names the abac_rules row that produced the refusal, so a
	// denial is traceable to a rule from the decision log alone.
	RuleCode string
	// Effect is the rule's effect (domain.EffectRequire / EffectForbid),
	// which is what distinguishes "a required condition was not met" from
	// "a forbidden condition was met" in the basis string.
	Effect string
	// Unevaluable is true when the rule could not be executed at all — an
	// operator this build does not implement. Reported separately because it
	// is an operator error in the RULE, not a property of the request, and
	// the caller logs it at a different level.
	Unevaluable bool
}

// Basis renders the decision_basis fragment for this denial, in the same
// `layer:detail=value` convention the RBAC, delegation and SoD layers use so
// one parser reads all of them.
func (d Denial) Basis() string {
	switch {
	case d.Unevaluable:
		return "abac:rule_unevaluable=" + d.RuleCode
	case d.Effect == domain.EffectForbid:
		return "abac:forbidden=" + d.RuleCode
	default:
		return "abac:require_failed=" + d.RuleCode
	}
}

// Evaluate returns the FIRST rule that refuses the request, or ok=false if
// none does.
//
// First, not all: the decision is binary and decision_basis names one reason,
// exactly as the SoD layer reports one conflicting action rather than every
// conflict. Rules arrive ordered by rule_code from the store, so which rule is
// reported for a given request is stable across evaluations rather than
// dependent on row order.
//
// attributes is what the calling service sent; a nil map is the normal case
// for a caller that sends none, and is not an error. It only produces a
// denial if a REQUIRE rule exists for the action — see the absence semantics
// on domain.EffectRequire.
func Evaluate(rules []domain.ABACRule, attributes map[string]string) (Denial, bool) {
	for _, rule := range rules {
		operands, known := domain.ABACOperators[rule.Operator]
		if !known {
			// An authorization condition nobody can evaluate has not been
			// met. Denying is the only safe reading, and the caller logs this
			// loudly because it is a defect in the rule, not in the request.
			return Denial{RuleCode: rule.RuleCode, Effect: rule.Effect, Unevaluable: true}, true
		}

		value, present := attributes[rule.AttributeKey]

		var operand string
		if rule.AttributeValue != nil {
			operand = *rule.AttributeValue
		}
		if operands == 1 && operand == "" {
			// Same reasoning as the unknown operator: a comparison with
			// nothing to compare against cannot be satisfied. CreateABACRule
			// refuses to store this, so reaching it means the row was written
			// around the API.
			return Denial{RuleCode: rule.RuleCode, Effect: rule.Effect, Unevaluable: true}, true
		}

		holds := conditionHolds(rule.Operator, value, present, operand)

		switch rule.Effect {
		case domain.EffectRequire:
			if !holds {
				return Denial{RuleCode: rule.RuleCode, Effect: rule.Effect}, true
			}
		case domain.EffectForbid:
			if holds {
				return Denial{RuleCode: rule.RuleCode, Effect: rule.Effect}, true
			}
		default:
			// Neither REQUIRE nor FORBID. There is no third safe reading of an
			// unknown effect on a deny-only layer: treating it as "no opinion"
			// would make a typo silently disable the control.
			return Denial{RuleCode: rule.RuleCode, Effect: rule.Effect, Unevaluable: true}, true
		}
	}
	return Denial{}, false
}

// conditionHolds answers whether the rule's condition is TRUE for this
// request. Whether a true condition permits or denies is the effect's job,
// not this function's.
//
// An attribute the caller did not send makes every condition except
// not_exists FALSE. Under REQUIRE that denies (a required condition that
// cannot be evaluated is not satisfied); under FORBID it permits (a condition
// that cannot be met cannot be violated). Both follow from this one rule
// rather than from per-operator special cases.
func conditionHolds(operator, value string, present bool, operand string) bool {
	switch operator {
	case "exists":
		return present
	case "not_exists":
		return !present
	}

	if !present {
		return false
	}

	switch operator {
	case "eq":
		return value == operand
	case "ne":
		return value != operand
	case "contains":
		return strings.Contains(value, operand)
	case "in":
		return inList(value, operand)
	case "not_in":
		return !inList(value, operand)
	case "lt", "lte", "gt", "gte":
		return compare(operator, value, operand)
	}
	return false
}

// inList splits a comma-separated operand, which is the shape a console form
// produces. Entries are trimmed so "A, B" and "A,B" mean the same thing —
// otherwise a stray space in the rule silently stops it matching, which on a
// FORBID rule means the control quietly does nothing.
func inList(value, operand string) bool {
	for _, candidate := range strings.Split(operand, ",") {
		if strings.TrimSpace(candidate) == value {
			return true
		}
	}
	return false
}

// compare orders value against operand NUMERICALLY when both parse as
// numbers, and lexicographically otherwise.
//
// Numeric first, because the conditions these operators exist for are
// thresholds — an amount, a count, a risk score — and a lexicographic
// comparison gets those wrong in the dangerous direction: as strings,
// "9" > "10000", so a rule forbidding amounts greater than 10000 would permit
// 9000000. Falling back to lexicographic keeps the operators usable for
// ordered non-numeric attributes (an ISO date, a version) rather than
// silently failing on them.
func compare(operator, value, operand string) bool {
	if lhs, lErr := strconv.ParseFloat(value, 64); lErr == nil {
		if rhs, rErr := strconv.ParseFloat(operand, 64); rErr == nil {
			switch operator {
			case "lt":
				return lhs < rhs
			case "lte":
				return lhs <= rhs
			case "gt":
				return lhs > rhs
			case "gte":
				return lhs >= rhs
			}
		}
	}
	switch operator {
	case "lt":
		return value < operand
	case "lte":
		return value <= operand
	case "gt":
		return value > operand
	case "gte":
		return value >= operand
	}
	return false
}
