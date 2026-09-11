package abac_test

import (
	"testing"

	"zoiko.io/authorization-svc/internal/abac"
	"zoiko.io/authorization-svc/internal/domain"
)

func rule(code, effect, key, op, value string) domain.ABACRule {
	r := domain.ABACRule{
		RuleCode:     code,
		ActionType:   "PAYMENT_APPROVE",
		Effect:       effect,
		AttributeKey: key,
		Operator:     op,
		ActiveFlag:   true,
	}
	if value != "" {
		r.AttributeValue = &value
	}
	return r
}

// TestEvaluate_NoRulesIsNoOp is the property that makes shipping this layer
// with an empty abac_rules table safe: with no rules declared, /v1/authorize
// behaves exactly as it did before layer 5 existed.
func TestEvaluate_NoRulesIsNoOp(t *testing.T) {
	for _, rules := range [][]domain.ABACRule{nil, {}} {
		if _, denied := abac.Evaluate(rules, map[string]string{"amount": "1"}); denied {
			t.Fatalf("an empty rule set denied a request")
		}
	}
}

func TestEvaluate_Operators(t *testing.T) {
	tests := []struct {
		name       string
		rule       domain.ABACRule
		attributes map[string]string
		wantDenied bool
	}{
		// ── REQUIRE ──────────────────────────────────────────────────────
		{
			name:       "require eq satisfied",
			rule:       rule("R1", domain.EffectRequire, "dual_approved", "eq", "true"),
			attributes: map[string]string{"dual_approved": "true"},
			wantDenied: false,
		},
		{
			name:       "require eq unsatisfied",
			rule:       rule("R1", domain.EffectRequire, "dual_approved", "eq", "true"),
			attributes: map[string]string{"dual_approved": "false"},
			wantDenied: true,
		},
		{
			// The bypass this closes: if an absent attribute passed, any
			// caller could evade a REQUIRE rule by omitting a JSON field.
			name:       "require with the attribute absent DENIES",
			rule:       rule("R1", domain.EffectRequire, "dual_approved", "eq", "true"),
			attributes: nil,
			wantDenied: true,
		},
		{
			name:       "require exists satisfied",
			rule:       rule("R2", domain.EffectRequire, "approval_ref", "exists", ""),
			attributes: map[string]string{"approval_ref": "AP-1"},
			wantDenied: false,
		},
		{
			name:       "require exists unsatisfied",
			rule:       rule("R2", domain.EffectRequire, "approval_ref", "exists", ""),
			attributes: map[string]string{"other": "x"},
			wantDenied: true,
		},

		// ── FORBID ───────────────────────────────────────────────────────
		{
			name:       "forbid eq matched",
			rule:       rule("F1", domain.EffectForbid, "channel", "eq", "SELF_SERVICE"),
			attributes: map[string]string{"channel": "SELF_SERVICE"},
			wantDenied: true,
		},
		{
			name:       "forbid eq not matched",
			rule:       rule("F1", domain.EffectForbid, "channel", "eq", "SELF_SERVICE"),
			attributes: map[string]string{"channel": "BRANCH"},
			wantDenied: false,
		},
		{
			// The mirror of the REQUIRE case above, and deliberately the
			// opposite outcome: a condition that cannot be evaluated has not
			// been met, so a FORBID rule cannot have been violated.
			name:       "forbid with the attribute absent PERMITS",
			rule:       rule("F1", domain.EffectForbid, "channel", "eq", "SELF_SERVICE"),
			attributes: nil,
			wantDenied: false,
		},
		{
			name:       "forbid not_exists matched — attribute missing",
			rule:       rule("F2", domain.EffectForbid, "approval_ref", "not_exists", ""),
			attributes: map[string]string{},
			wantDenied: true,
		},

		// ── list membership ──────────────────────────────────────────────
		{
			name:       "forbid in, member",
			rule:       rule("F3", domain.EffectForbid, "channel", "in", "SELF_SERVICE,API"),
			attributes: map[string]string{"channel": "API"},
			wantDenied: true,
		},
		{
			// A stray space in the rule must not silently stop a FORBID rule
			// matching — that is a control that quietly does nothing.
			name:       "forbid in tolerates spaces in the operand list",
			rule:       rule("F3", domain.EffectForbid, "channel", "in", "SELF_SERVICE, API"),
			attributes: map[string]string{"channel": "API"},
			wantDenied: true,
		},
		{
			name:       "require in, not a member",
			rule:       rule("R3", domain.EffectRequire, "channel", "in", "BRANCH,BACK_OFFICE"),
			attributes: map[string]string{"channel": "API"},
			wantDenied: true,
		},
		{
			name:       "require not_in, absent from the list",
			rule:       rule("R4", domain.EffectRequire, "channel", "not_in", "SELF_SERVICE"),
			attributes: map[string]string{"channel": "BRANCH"},
			wantDenied: false,
		},

		// ── ordering ─────────────────────────────────────────────────────
		{
			// The reason compare() parses numbers first: as strings
			// "9000000" < "10000", so a lexicographic comparison would
			// PERMIT this and let a nine-million payment through a
			// ten-thousand threshold.
			name:       "forbid gt compares numerically, not lexicographically",
			rule:       rule("F4", domain.EffectForbid, "amount", "gt", "10000"),
			attributes: map[string]string{"amount": "9000000"},
			wantDenied: true,
		},
		{
			name:       "forbid gt under the threshold",
			rule:       rule("F4", domain.EffectForbid, "amount", "gt", "10000"),
			attributes: map[string]string{"amount": "9000"},
			wantDenied: false,
		},
		{
			name:       "gte is inclusive",
			rule:       rule("F5", domain.EffectForbid, "amount", "gte", "10000"),
			attributes: map[string]string{"amount": "10000"},
			wantDenied: true,
		},
		{
			// Non-numeric ordered attributes still work, so the operators are
			// not numeric-only.
			name:       "lt falls back to lexicographic for ISO dates",
			rule:       rule("R5", domain.EffectRequire, "value_date", "lt", "2026-10-01"),
			attributes: map[string]string{"value_date": "2026-09-15"},
			wantDenied: false,
		},
		{
			name:       "contains",
			rule:       rule("F6", domain.EffectForbid, "classification", "contains", "RESTRICTED"),
			attributes: map[string]string{"classification": "INTERNAL_RESTRICTED"},
			wantDenied: true,
		},

		// ── unevaluable ──────────────────────────────────────────────────
		{
			// Fail closed and loud: a condition nobody can evaluate has not
			// been met. CreateABACRule refuses to store this, so reaching it
			// means the row was written around the API.
			name:       "unknown operator denies",
			rule:       rule("BAD1", domain.EffectRequire, "amount", "approximately", "10000"),
			attributes: map[string]string{"amount": "10000"},
			wantDenied: true,
		},
		{
			name:       "unknown effect denies",
			rule:       rule("BAD2", "MAYBE", "amount", "eq", "10000"),
			attributes: map[string]string{"amount": "10000"},
			wantDenied: true,
		},
		{
			name:       "comparison operator with no operand denies",
			rule:       rule("BAD3", domain.EffectRequire, "amount", "gt", ""),
			attributes: map[string]string{"amount": "10000"},
			wantDenied: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, denied := abac.Evaluate([]domain.ABACRule{tc.rule}, tc.attributes)
			if denied != tc.wantDenied {
				t.Fatalf("denied = %v, want %v", denied, tc.wantDenied)
			}
		})
	}
}

// TestEvaluate_BasisNamesTheRule pins that a denial is traceable to a rule
// from the decision log alone — decision_basis is the only thing an operator
// has when asking why a request was refused.
func TestEvaluate_BasisNamesTheRule(t *testing.T) {
	tests := []struct {
		name      string
		rule      domain.ABACRule
		attrs     map[string]string
		wantBasis string
	}{
		{
			name:      "require",
			rule:      rule("DUAL_APPROVAL_REQUIRED", domain.EffectRequire, "dual_approved", "eq", "true"),
			attrs:     map[string]string{"dual_approved": "false"},
			wantBasis: "abac:require_failed=DUAL_APPROVAL_REQUIRED",
		},
		{
			name:      "forbid",
			rule:      rule("NO_SELF_SERVICE_RELEASE", domain.EffectForbid, "channel", "eq", "SELF_SERVICE"),
			attrs:     map[string]string{"channel": "SELF_SERVICE"},
			wantBasis: "abac:forbidden=NO_SELF_SERVICE_RELEASE",
		},
		{
			name:      "unevaluable",
			rule:      rule("TYPO_RULE", domain.EffectRequire, "amount", "approximately", "1"),
			attrs:     map[string]string{"amount": "1"},
			wantBasis: "abac:rule_unevaluable=TYPO_RULE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			denial, denied := abac.Evaluate([]domain.ABACRule{tc.rule}, tc.attrs)
			if !denied {
				t.Fatal("expected a denial")
			}
			if got := denial.Basis(); got != tc.wantBasis {
				t.Fatalf("basis = %q, want %q", got, tc.wantBasis)
			}
		})
	}
}

// TestEvaluate_ReportsTheFirstRefusalDeterministically — the decision is
// binary and decision_basis names one reason, so which rule is reported has to
// be stable rather than dependent on row order. The store returns rules
// ordered by rule_code; this pins that Evaluate respects that order rather
// than, say, preferring FORBID rules.
func TestEvaluate_ReportsTheFirstRefusalDeterministically(t *testing.T) {
	rules := []domain.ABACRule{
		rule("A_FIRST", domain.EffectRequire, "missing_one", "exists", ""),
		rule("B_SECOND", domain.EffectForbid, "channel", "eq", "SELF_SERVICE"),
	}
	denial, denied := abac.Evaluate(rules, map[string]string{"channel": "SELF_SERVICE"})
	if !denied {
		t.Fatal("expected a denial")
	}
	if denial.RuleCode != "A_FIRST" {
		t.Fatalf("reported %q, want the first refusing rule A_FIRST", denial.RuleCode)
	}
}

// TestEvaluate_InactiveRulesAreTheStoresJob is a documentation test, not a
// behaviour one: active_flag is filtered in FindABACRules' SQL, and Evaluate
// deliberately does NOT re-check it. Pinning that here means a future change
// that starts passing inactive rules in fails loudly rather than having them
// silently enforced.
func TestEvaluate_InactiveRulesAreTheStoresJob(t *testing.T) {
	r := rule("RETIRED", domain.EffectForbid, "channel", "eq", "SELF_SERVICE")
	r.ActiveFlag = false
	if _, denied := abac.Evaluate([]domain.ABACRule{r}, map[string]string{"channel": "SELF_SERVICE"}); !denied {
		t.Fatal("Evaluate now filters inactive rules itself — if that is intended, " +
			"FindABACRules' active_flag predicate is the place to remove, not to duplicate")
	}
}
