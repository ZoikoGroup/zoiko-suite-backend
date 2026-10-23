package kernel

import (
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

// Rule-set statuses (standard §6.2).
const (
	RuleDraft      = "DRAFT"
	RuleTest       = "TEST"
	RuleApproved   = "APPROVED"
	RuleActive     = "ACTIVE"
	RuleSuperseded = "SUPERSEDED"
	RuleRetired    = "RETIRED"
)

// Sides of a journal line.
const (
	Debit  = "DEBIT"
	Credit = "CREDIT"
)

// AccountRef names an account either directly or through a mapping key. A rule never carries both.
type AccountRef struct {
	AccountCode string `json:"account_code,omitempty"`
	MappingKey  string `json:"mapping_key,omitempty"`
}

func (a AccountRef) valid() bool { return (a.AccountCode != "") != (a.MappingKey != "") }
func (a AccountRef) String() string {
	if a.AccountCode != "" {
		return "account:" + a.AccountCode
	}
	return "mapping:" + a.MappingKey
}

// Condition is a deterministic selection predicate over the event's facts.
type Condition struct {
	Fact  string `json:"fact"`
	Op    string `json:"op"` // eq ne in gt ge lt le exists
	Value any    `json:"value,omitempty"`
}

// LineTemplate generates one journal line from the event.
type LineTemplate struct {
	LineRef     string            `json:"line_ref"`
	Side        string            `json:"side"`
	Account     AccountRef        `json:"account"`
	Amount      string            `json:"amount"`
	Description string            `json:"description,omitempty"`
	Dimensions  map[string]string `json:"dimensions,omitempty"`
	When        []Condition       `json:"when,omitempty"`
	KeepZero    bool              `json:"keep_zero,omitempty"`
}

// Dimension rule modes (standard §13).
const (
	DimRequired   = "REQUIRED"
	DimOptional   = "OPTIONAL"
	DimDerived    = "DERIVED"
	DimProhibited = "PROHIBITED"
)

// DimensionRule says how a dimension is treated on the lines of a rule.
type DimensionRule struct {
	Dimension string `json:"dimension"`
	Mode      string `json:"mode"`
	// From is where a DERIVED value comes from: "$dimensions.<name>" or "$facts.<name>" or a literal default.
	From string `json:"from,omitempty"`
}

// TaxDependency makes a rule wait for an authoritative tax result: without these facts no posting is proposed (AK-TAX).
type TaxDependency struct {
	RequiredFacts []string `json:"required_facts"`
}

// RuleContent is the declarative body of a rule set. Everything here is covered by the content hash.
type RuleContent struct {
	Conditions       []Condition     `json:"conditions,omitempty"`
	LineTemplates    []LineTemplate  `json:"line_templates"`
	RoundingPolicyID string          `json:"rounding_policy_id"`
	RoundingAccount  *AccountRef     `json:"rounding_account,omitempty"`
	DimensionRules   []DimensionRule `json:"dimension_rules,omitempty"`
	TaxDependency    *TaxDependency  `json:"tax_dependency,omitempty"`
	FXPolicyID       string          `json:"fx_policy_id,omitempty"`
	ApprovalPolicyID string          `json:"approval_policy_id,omitempty"`
}

// TestCase is one regression case of a rule set's test pack: an event, what the directory resolves, and what must come
// out (lines or an exception family).
type TestCase struct {
	Name       string            `json:"name"`
	Event      TestEvent         `json:"event"`
	Accounts   map[string]string `json:"accounts,omitempty"` // mapping key -> account code
	FX         *FXBasis          `json:"fx,omitempty"`
	Functional string            `json:"functional_currency,omitempty"`
	MinorUnits map[string]int    `json:"minor_units,omitempty"`
	Expect     TestExpectation   `json:"expect"`
}

// TestEvent is the part of an event a rule looks at.
type TestEvent struct {
	EventType           string            `json:"event_type"`
	TransactionCurrency string            `json:"transaction_currency"`
	Facts               map[string]string `json:"facts"`
	Dimensions          map[string]string `json:"dimensions,omitempty"`
	CounterpartyID      string            `json:"counterparty_id,omitempty"`
}

// TestExpectation is what a case asserts.
type TestExpectation struct {
	Lines     []ExpectedLine `json:"lines,omitempty"`
	Exception string         `json:"exception,omitempty"` // a family such as AK-DIM
}

// ExpectedLine is one line a case expects, in order.
type ExpectedLine struct {
	Side        string `json:"side"`
	AccountCode string `json:"account_code"`
	Amount      string `json:"amount"`
}

// RuleSet is one immutable version of a posting rule set.
type RuleSet struct {
	RuleSetID       string      `json:"posting_rule_set_id"`
	TenantID        string      `json:"tenant_id"`
	Name            string      `json:"name"`
	Domain          string      `json:"domain"`
	EventType       string      `json:"event_type"`
	BookType        string      `json:"book_type"`
	FrameworkID     string      `json:"accounting_framework_id"`
	Version         string      `json:"version"`
	Status          string      `json:"status"`
	EffectiveFrom   time.Time   `json:"effective_from"`
	EffectiveTo     *time.Time  `json:"effective_to,omitempty"`
	Content         RuleContent `json:"content"`
	ContentHash     string      `json:"content_hash"`
	TestPack        []TestCase  `json:"test_pack"`
	TestPackID      string      `json:"test_pack_id"`
	TestsPassedHash string      `json:"tests_passed_hash,omitempty"`
	SourceAuthority []string    `json:"source_authority_refs,omitempty"`
	CreatedBy       string      `json:"created_by"`
	ApprovedBy      string      `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time  `json:"approved_at,omitempty"`
	ActivatedBy     string      `json:"activated_by,omitempty"`
	ActivatedAt     *time.Time  `json:"activated_at,omitempty"`
	RowVersion      int64       `json:"row_version"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

var (
	semverRE  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	lineRefRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

// HashContent returns the hash that binds a rule set's content and test pack: the test pack that passed is the one that
// is approved.
func HashContent(c RuleContent, pack []TestCase) string {
	b, _ := json.Marshal(struct {
		C RuleContent
		P []TestCase
	}{c, pack})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks a rule set to be created: shape, expressions, balanced structure and a non-empty test pack.
func (r *RuleSet) Validate() error {
	bad := func(f, why string) error { return exc(FamRule, "RULE_SET_INVALID", "%s: %s", f, why).with("field", f) }
	if !codeRE.MatchString(r.Name) {
		return bad("name", "1-63 letters, digits, dot, dash or underscore")
	}
	if !codeRE.MatchString(r.Domain) {
		return bad("domain", "the owning domain")
	}
	if !eventTypeRE.MatchString(r.EventType) {
		return bad("event_type", "a dotted type such as AR.INVOICE_ISSUED")
	}
	known := false
	for _, t := range BookTypes {
		known = known || t == r.BookType
	}
	if !known {
		return bad("book_type", "one of "+strings.Join(BookTypes, ", "))
	}
	if !codeRE.MatchString(r.FrameworkID) {
		return bad("accounting_framework_id", "a controlled identifier")
	}
	if !semverRE.MatchString(r.Version) {
		return bad("version", "a semantic version such as 1.0.0")
	}
	if r.EffectiveFrom.IsZero() {
		return bad("effective_from", "required")
	}
	if r.EffectiveTo != nil && !r.EffectiveTo.After(r.EffectiveFrom) {
		return bad("effective_to", "after effective_from")
	}
	if len(r.TestPack) == 0 {
		return bad("test_pack", "a rule set needs regression cases before it can be approved")
	}
	if !codeRE.MatchString(r.TestPackID) {
		return bad("test_pack_id", "required")
	}
	return r.Content.Validate()
}

// Validate checks the body of a rule set.
func (c *RuleContent) Validate() error {
	bad := func(f, why string) error {
		return exc(FamRule, "RULE_CONTENT_INVALID", "%s: %s", f, why).with("field", f)
	}
	if _, ok := BuiltinRoundingPolicies[c.RoundingPolicyID]; !ok {
		return bad("rounding_policy_id", "a known rounding policy")
	}
	pol := BuiltinRoundingPolicies[c.RoundingPolicyID]
	if pol.ResidualTolerance > 0 && (c.RoundingAccount == nil || !c.RoundingAccount.valid()) {
		return bad("rounding_account", "a rounding policy that allows a residual needs the account it is posted to")
	}
	if c.RoundingAccount != nil && !c.RoundingAccount.valid() {
		return bad("rounding_account", "exactly one of account_code or mapping_key")
	}
	if len(c.LineTemplates) < 2 || len(c.LineTemplates) > 60 {
		return bad("line_templates", "2-60 line templates")
	}
	sides := map[string]int{}
	refs := map[string]bool{}
	for i, t := range c.LineTemplates {
		at := fmt.Sprintf("line_templates[%d].", i)
		if !lineRefRE.MatchString(t.LineRef) || refs[t.LineRef] {
			return bad(at+"line_ref", "a unique reference")
		}
		refs[t.LineRef] = true
		if t.Side != Debit && t.Side != Credit {
			return bad(at+"side", "DEBIT or CREDIT")
		}
		sides[t.Side]++
		if !t.Account.valid() {
			return bad(at+"account", "exactly one of account_code or mapping_key")
		}
		if _, err := ParseExpr(t.Amount); err != nil {
			return bad(at+"amount", err.Error())
		}
		if err := validateConditions(t.When); err != nil {
			return bad(at+"when", err.Error())
		}
		for d, v := range t.Dimensions {
			if !codeRE.MatchString(d) || strings.TrimSpace(v) == "" || len(v) > 120 {
				return bad(at+"dimensions."+d, "a dimension code and a literal or $dimensions.x / $facts.x value")
			}
		}
	}
	if sides[Debit] == 0 || sides[Credit] == 0 {
		return bad("line_templates", "a journal needs at least one debit and one credit template")
	}
	if err := validateConditions(c.Conditions); err != nil {
		return bad("conditions", err.Error())
	}
	seen := map[string]bool{}
	for i, d := range c.DimensionRules {
		at := fmt.Sprintf("dimension_rules[%d].", i)
		if !codeRE.MatchString(d.Dimension) || seen[d.Dimension] {
			return bad(at+"dimension", "a unique dimension code")
		}
		seen[d.Dimension] = true
		switch d.Mode {
		case DimRequired, DimOptional, DimProhibited:
		case DimDerived:
			if strings.TrimSpace(d.From) == "" {
				return bad(at+"from", "a derived dimension says where its value comes from")
			}
		default:
			return bad(at+"mode", "REQUIRED, OPTIONAL, DERIVED or PROHIBITED")
		}
	}
	if c.TaxDependency != nil {
		if len(c.TaxDependency.RequiredFacts) == 0 || len(c.TaxDependency.RequiredFacts) > 10 {
			return bad("tax_dependency.required_facts", "1-10 facts")
		}
		for _, f := range c.TaxDependency.RequiredFacts {
			if !identRE.MatchString(f) {
				return bad("tax_dependency.required_facts", f+" is not a fact name")
			}
		}
	}
	return nil
}

var condOps = map[string]bool{"eq": true, "ne": true, "in": true, "gt": true, "ge": true, "lt": true, "le": true, "exists": true}

func validateConditions(cs []Condition) error {
	if len(cs) > 20 {
		return fmt.Errorf("at most 20 conditions")
	}
	for i, c := range cs {
		if c.Fact == "" || len(c.Fact) > 80 {
			return fmt.Errorf("condition %d names no fact", i)
		}
		if !condOps[c.Op] {
			return fmt.Errorf("condition %d: unknown operator %q", i, c.Op)
		}
		switch c.Op {
		case "exists":
		case "in":
			if _, ok := c.Value.([]any); !ok {
				return fmt.Errorf("condition %d: in needs a list", i)
			}
		case "gt", "ge", "lt", "le":
			if s, ok := c.Value.(string); !ok {
				return fmt.Errorf("condition %d: %s needs a decimal string", i, c.Op)
			} else if _, ok := ParseDecimal(s); !ok {
				return fmt.Errorf("condition %d: %q is not a decimal", i, s)
			}
		default:
			if c.Value == nil {
				return fmt.Errorf("condition %d: %s needs a value", i, c.Op)
			}
		}
	}
	return nil
}

// lookup resolves a condition's fact name against the event.
func lookup(ev *AccountingEvent, name string) (string, bool) {
	switch name {
	case "event_type":
		return ev.EventType, true
	case "transaction_currency":
		return ev.TransactionCurrency, ev.TransactionCurrency != ""
	case "legal_entity_id":
		return ev.LegalEntityID, true
	case "counterparty_id":
		return ev.CounterpartyID, ev.CounterpartyID != ""
	case "source_system":
		return ev.SourceSystem, true
	}
	if strings.HasPrefix(name, "dimensions.") {
		v, ok := ev.Dimensions[strings.TrimPrefix(name, "dimensions.")]
		return v, ok
	}
	v, ok := ev.Facts[strings.TrimPrefix(name, "facts.")]
	return v, ok
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return new(big.Rat).SetFloat64(x).FloatString(0)
	case bool:
		return fmt.Sprint(x)
	}
	return fmt.Sprint(v)
}

// evalConditions reports whether every condition holds.
func evalConditions(ev *AccountingEvent, cs []Condition) bool {
	for _, c := range cs {
		got, present := lookup(ev, c.Fact)
		switch c.Op {
		case "exists":
			if !present {
				return false
			}
		case "eq":
			if !present || got != scalar(c.Value) {
				return false
			}
		case "ne":
			if present && got == scalar(c.Value) {
				return false
			}
		case "in":
			in := false
			for _, v := range c.Value.([]any) {
				if present && got == scalar(v) {
					in = true
				}
			}
			if !in {
				return false
			}
		default:
			a, ok1 := ParseDecimal(got)
			b, ok2 := ParseDecimal(scalar(c.Value))
			if !present || !ok1 || !ok2 {
				return false
			}
			cmp := a.Cmp(b)
			switch c.Op {
			case "gt":
				if cmp <= 0 {
					return false
				}
			case "ge":
				if cmp < 0 {
					return false
				}
			case "lt":
				if cmp >= 0 {
					return false
				}
			case "le":
				if cmp > 0 {
					return false
				}
			}
		}
	}
	return true
}

// Scope is what two rule sets must not both claim on the same date: an event type for a book type and framework.
func (r *RuleSet) Scope() string { return r.EventType + "|" + r.BookType + "|" + r.FrameworkID }

// Covers reports whether the rule set applies on the date.
func (r *RuleSet) Covers(d time.Time) bool {
	return r.Status == RuleActive && !d.Before(r.EffectiveFrom) && (r.EffectiveTo == nil || d.Before(*r.EffectiveTo))
}

// Overlaps reports whether two rule sets claim the same scope on any common date.
func (r *RuleSet) Overlaps(o *RuleSet) bool {
	if r.Scope() != o.Scope() || r.RuleSetID == o.RuleSetID {
		return false
	}
	aEnd, bEnd := farFuture, farFuture
	if r.EffectiveTo != nil {
		aEnd = *r.EffectiveTo
	}
	if o.EffectiveTo != nil {
		bEnd = *o.EffectiveTo
	}
	return r.EffectiveFrom.Before(bEnd) && o.EffectiveFrom.Before(aEnd)
}

var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// legalTransitions is the rule-set lifecycle. Content never changes once a rule set leaves DRAFT.
var legalTransitions = map[string][]string{
	RuleDraft:      {RuleTest},
	RuleTest:       {RuleDraft, RuleApproved},
	RuleApproved:   {RuleActive, RuleDraft},
	RuleActive:     {RuleSuperseded, RuleRetired},
	RuleSuperseded: {RuleRetired},
}

// CanTransition reports whether a status change is on the lifecycle.
func CanTransition(from, to string) bool {
	for _, t := range legalTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// sortedKeys returns a map's keys in order, for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
