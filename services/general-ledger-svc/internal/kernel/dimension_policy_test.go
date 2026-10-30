package kernel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// policyRef is a chart that serves status, scope, dimension policy and replacement per account.
type policyRef struct {
	active map[string]bool
	scope  map[string]AccountScope
	maps   map[string]string
}

func (r policyRef) AccountFacts(_ context.Context, _, code string) (bool, bool, bool, error) {
	a, ok := r.active[code]
	return a, true, ok, nil
}
func (r policyRef) MappedAccount(_ context.Context, _, key string, _ time.Time) (string, bool, error) {
	c, ok := r.maps[key]
	return c, ok, nil
}
func (r policyRef) DimensionAllowed(context.Context, string, string, string, string, time.Time) (bool, error) {
	return true, nil
}
func (r policyRef) AccountApplicability(_ context.Context, _, code string) (AccountScope, error) {
	return r.scope[code], nil
}

func TestRetiredAccountMapsForwardToItsReplacement(t *testing.T) {
	ref := policyRef{
		active: map[string]bool{"4000": false, "4001": false, "4002": true, "5000": false, "5001": true, "6000": false, "7000": true},
		scope: map[string]AccountScope{
			"4000": {ReplacementAccount: "4001"}, "4001": {ReplacementAccount: "4002"}, // a chain: 4000 -> 4001 -> 4002
			"5000": {}, // retired with no replacement
			"6000": {ReplacementAccount: "6000"},
			"7000": {DimensionPolicy: map[string]string{"cost_center": "REQUIRED"}},
		},
		maps: map[string]string{"REV": "4000"},
	}
	d := TenantDirectory{Tenant: tenant, Ref: ref}
	ctx := context.Background()

	info, err := d.ResolveAccount(ctx, AccountRef{MappingKey: "REV"}, "", d0)
	if err != nil || info.Code != "4002" || !info.Active || len(info.ReplacedFrom) != 2 || info.ReplacedFrom[0] != "4000" || info.ReplacedFrom[1] != "4001" {
		t.Fatalf("a chain is followed to its end, and the evidence names the accounts passed: %+v %v", info, err)
	}
	// a retired account with nothing to map to is still refused by the engine as inactive
	if info, err = d.ResolveAccount(ctx, AccountRef{AccountCode: "5000"}, "", d0); err != nil || info.Active || len(info.ReplacedFrom) != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	// an account that loops on itself is refused rather than followed
	if _, err = d.ResolveAccount(ctx, AccountRef{AccountCode: "6000"}, "", d0); err == nil || !strings.Contains(err.Error(), "does not end") {
		t.Fatalf("%v", err)
	}
	// an open account is never replaced, whatever it says
	ref.scope["7000"] = AccountScope{ReplacementAccount: "5001", DimensionPolicy: map[string]string{"cost_center": "REQUIRED"}}
	if info, err = d.ResolveAccount(ctx, AccountRef{AccountCode: "7000"}, "", d0); err != nil || info.Code != "7000" || info.DimensionPolicy["cost_center"] != "REQUIRED" {
		t.Fatalf("%+v %v", info, err)
	}
	// a replacement that cannot be read is an error that names both accounts
	ref.scope["4001"] = AccountScope{ReplacementAccount: "9999"}
	if _, err = d.ResolveAccount(ctx, AccountRef{AccountCode: "4000"}, "", d0); err == nil || !strings.Contains(err.Error(), "4001") || !strings.Contains(err.Error(), "9999") {
		t.Fatalf("%v", err)
	}
}

func TestAccountPostedToAReplacementLandsThereAndSaysSo(t *testing.T) {
	ref := policyRef{
		active: map[string]bool{"1100": true, "4000": false, "4100": true, "2200": true},
		scope:  map[string]AccountScope{"4000": {ReplacementAccount: "4100"}},
		maps:   map[string]string{"AR": "1100", "REV": "4000", "VAT": "2200"},
	}
	d := TenantDirectory{Tenant: tenant, Ref: ref, Units: unitsStub{}}
	p, err := Evaluate(context.Background(), EvalInput{Event: event(okFacts()), Book: book(), Rule: rule(invoiceContent()), PostingDate: d0.AddDate(0, 1, 0), Directory: d})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range p.Lines {
		got[l.LineRef] = l.AccountCode
	}
	if got["L2"] != "4100" || got["L1"] != "1100" {
		t.Fatalf("revenue lands on the account that replaced the retired one: %v", got)
	}
	var said bool
	for _, s := range p.Trace {
		if s.Step == "coa" && strings.Contains(s.Detail, "4000") && strings.Contains(s.Detail, "4100") && strings.Contains(s.Detail, "L2") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the evidence says the posting was mapped forward: %+v", p.Trace)
	}
}

type unitsStub struct{}

func (unitsStub) MinorUnits(context.Context, string, time.Time) (int, error) { return 2, nil }

func TestScopeValidation(t *testing.T) {
	for name, sc := range map[string]AccountScope{
		"unknown mode":    {DimensionPolicy: map[string]string{"cost_center": "MAYBE"}},
		"bad dimension":   {DimensionPolicy: map[string]string{"not a code": "REQUIRED"}},
		"replaces itself": {ReplacementAccount: "1100"},
		"bad replacement": {ReplacementAccount: "no good"},
		"too many policies": func() AccountScope {
			m := map[string]string{}
			for i := 0; i < 25; i++ {
				m["d"+strings.Repeat("x", i)] = "REQUIRED"
			}
			return AccountScope{DimensionPolicy: m}
		}(),
	} {
		if err := ValidateScope("1100", sc); err == nil {
			t.Errorf("%s must be refused", name)
		} else if e, ok := AsException(err); !ok || e.Family != FamCOA || e.Code != "SCOPE_INVALID" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ValidateScope("1100", AccountScope{DimensionPolicy: map[string]string{"cost_center": "REQUIRED", "project": "OPTIONAL", "fund": "PROHIBITED"}, ReplacementAccount: "1101"}); err != nil {
		t.Fatal(err)
	}
}

// Rule-driven combinations (§13): where a line carries a dimension it must carry another, or must not carry one.
func TestDimensionCombinationsAreEnforcedPerLine(t *testing.T) {
	build := func(combos ...DimensionCombo) *RuleSet {
		c := invoiceContent()
		c.LineTemplates[1].Dimensions = map[string]string{"project": "$dimensions.project", "cost_center": "$dimensions.cost_center", "department": "$dimensions.department"}
		c.DimensionCombos = combos
		return rule(c)
	}
	ev := func(dims map[string]string) *AccountingEvent {
		e := event(okFacts())
		e.Dimensions = dims
		e.PayloadHash = e.ComputeHash()
		return e
	}
	combo := DimensionCombo{When: DimensionMatch{Dimension: "project"}, Require: []string{"cost_center"}, Forbid: []string{"department"}}
	rs := build(combo)
	if err := rs.Validate(); err != nil {
		t.Fatal(err)
	}

	if _, err := eval(t, ev(map[string]string{"project": "P1"}), rs, dir(), nil, nil); err == nil {
		t.Fatal("a project with no cost centre must be refused")
	} else {
		wantFam(t, err, FamDim, "DIMENSION_COMBINATION_REQUIRED")
	}
	if _, err := eval(t, ev(map[string]string{"project": "P1", "cost_center": "CC1"}), rs, dir(), nil, nil); err != nil {
		t.Fatalf("the required companion is enough: %v", err)
	}
	_, err := eval(t, ev(map[string]string{"project": "P1", "cost_center": "CC1", "department": "D1"}), rs, dir(), nil, nil)
	wantFam(t, err, FamDim, "DIMENSION_COMBINATION_FORBIDDEN")
	if _, err := eval(t, ev(map[string]string{"department": "D1"}), rs, dir(), nil, nil); err != nil {
		t.Fatalf("a combination about a project says nothing where there is none: %v", err)
	}

	// a combination about one value only
	valued := build(DimensionCombo{When: DimensionMatch{Dimension: "project", Value: "P1"}, Require: []string{"cost_center"}})
	if _, err := eval(t, ev(map[string]string{"project": "P2"}), valued, dir(), nil, nil); err != nil {
		t.Fatalf("another project is not bound by it: %v", err)
	}
	_, err = eval(t, ev(map[string]string{"project": "P1"}), valued, dir(), nil, nil)
	wantFam(t, err, FamDim, "DIMENSION_COMBINATION_REQUIRED")

	// the combinations are part of the rule's content hash: changing one is a new version of the rule
	if build(combo).ContentHash == build().ContentHash {
		t.Fatal("the content hash must cover the combinations")
	}

	for name, cb := range map[string]DimensionCombo{
		"nothing required or forbidden": {When: DimensionMatch{Dimension: "project"}},
		"bad dimension":                 {When: DimensionMatch{Dimension: "not a code"}, Require: []string{"x"}},
		"about itself":                  {When: DimensionMatch{Dimension: "project"}, Require: []string{"project"}},
		"required and forbidden":        {When: DimensionMatch{Dimension: "project"}, Require: []string{"fund"}, Forbid: []string{"fund"}},
	} {
		if err := build(cb).Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
