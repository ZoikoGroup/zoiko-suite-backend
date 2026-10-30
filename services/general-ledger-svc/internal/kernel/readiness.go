package kernel

import (
	"context"
	"sort"
	"time"
)

// Rollout readiness answers one question before a rule set is switched on: can everything it names actually resolve for this
// tenant? A rule that names a mapping key nobody mapped, or a dimension nobody registered, does not fail when it is approved
// or activated; it fails on the first event that reaches it (AK-COA MAPPING_MISSING, AK-DIM). Readiness finds those gaps
// first. It reads reference data only and writes nothing.

// Readiness item states.
const (
	ReadyOK              = "OK"
	ReadyUnmapped        = "UNMAPPED"         // no mapping for the key is in force on the date
	ReadyAccountMissing  = "ACCOUNT_MISSING"  // the mapped account is not in the chart
	ReadyAccountInactive = "ACCOUNT_INACTIVE" // the mapped account is closed
	ReadyAccountHeader   = "ACCOUNT_NOT_POSTABLE"
	ReadyDimMissing      = "DIMENSION_MISSING" // the rule names a dimension that is not registered
	ReadyDimRetired      = "DIMENSION_RETIRED"
	ReadyDimNoValues     = "DIMENSION_NO_VALUES" // a REQUIRED dimension with no value in force
)

// ReadinessItem is one thing a rule set needs and whether it is there.
type ReadinessItem struct {
	Kind     string   `json:"kind"` // MAPPING | DIMENSION
	Key      string   `json:"key"`
	State    string   `json:"state"`
	Account  string   `json:"account_code,omitempty"` // for a mapping, what it resolves to
	UsedBy   []string `json:"used_by"`                // event types of the rules that name it
	Blocking bool     `json:"blocking"`
	Remedy   string   `json:"remedy,omitempty"`
}

// ReadinessReport is the result for a set of rules on a date.
type ReadinessReport struct {
	On        string          `json:"on"`
	Source    string          `json:"source"` // "pack:<name>" or "active-rule-sets"
	RuleCount int             `json:"rule_count"`
	Items     []ReadinessItem `json:"items"`
	Ready     bool            `json:"ready"`
	Blocking  int             `json:"blocking"`
}

type need struct {
	used  map[string]bool
	reqd  bool // a dimension the rule requires a value for
	isDim bool
}

// RuleNeeds lists the mapping keys and dimensions one rule's content names, with the event types that use them.
func collectNeeds(into map[string]*need, eventType string, c RuleContent) {
	add := func(key string, dim, required bool) {
		if key == "" {
			return
		}
		k := key
		if dim {
			k = "dim:" + key
		} else {
			k = "map:" + key
		}
		n := into[k]
		if n == nil {
			n = &need{used: map[string]bool{}, isDim: dim}
			into[k] = n
		}
		n.used[eventType] = true
		if required {
			n.reqd = true
		}
	}
	for _, l := range c.LineTemplates {
		add(l.Account.MappingKey, false, false)
		for d := range l.Dimensions {
			add(d, true, false)
		}
	}
	if c.RoundingAccount != nil {
		add(c.RoundingAccount.MappingKey, false, false)
	}
	if c.RealizedFX != nil {
		add(c.RealizedFX.Gain.MappingKey, false, false)
		add(c.RealizedFX.Loss.MappingKey, false, false)
	}
	for _, dr := range c.DimensionRules {
		if dr.Mode == DimProhibited {
			continue
		}
		add(dr.Dimension, true, dr.Mode == DimRequired)
	}
}

// CheckReadiness evaluates what the given rules need against the tenant's reference data on a date.
func CheckReadiness(ctx context.Context, tenant, source string, rules []RuleSet, ref RefData, repo Repo, on time.Time) (*ReadinessReport, error) {
	needs := map[string]*need{}
	for _, r := range rules {
		collectNeeds(needs, r.EventType, r.Content)
	}
	dims, err := repo.ListDimensions(ctx, tenant)
	if err != nil {
		return nil, err
	}
	registered := map[string]Dimension{}
	for _, d := range dims {
		registered[d.DimensionCode] = d
	}

	rep := &ReadinessReport{On: on.Format("2006-01-02"), Source: source, RuleCount: len(rules), Items: []ReadinessItem{}}
	keys := make([]string, 0, len(needs))
	for k := range needs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := needs[k]
		it := ReadinessItem{Kind: "MAPPING", Key: k[4:], UsedBy: sortedNeedKeys(n.used)}
		if n.isDim {
			it.Kind = "DIMENSION"
			if err := checkDimension(ctx, tenant, &it, n, registered, repo, on); err != nil {
				return nil, err
			}
		} else if err := checkMapping(ctx, tenant, &it, ref, on); err != nil {
			return nil, err
		}
		if it.Blocking {
			rep.Blocking++
		}
		rep.Items = append(rep.Items, it)
	}
	rep.Ready = rep.Blocking == 0
	return rep, nil
}

func checkMapping(ctx context.Context, tenant string, it *ReadinessItem, ref RefData, on time.Time) error {
	code, found, err := ref.MappedAccount(ctx, tenant, it.Key, on)
	if err != nil {
		return err
	}
	if !found {
		it.State, it.Blocking = ReadyUnmapped, true
		it.Remedy = "POST /v1/account-mappings with mapping_key " + it.Key + " effective on or before the date"
		return nil
	}
	it.Account = code
	active, postable, exists, err := ref.AccountFacts(ctx, tenant, code)
	if err != nil {
		return err
	}
	switch {
	case !exists:
		it.State, it.Blocking = ReadyAccountMissing, true
		it.Remedy = "create account " + code + " in the chart of accounts or remap " + it.Key
	case !active:
		it.State, it.Blocking = ReadyAccountInactive, true
		it.Remedy = "reopen account " + code + " or remap " + it.Key
	case !postable:
		it.State, it.Blocking = ReadyAccountHeader, true
		it.Remedy = "account " + code + " is a header account; map " + it.Key + " to a postable account"
	default:
		it.State = ReadyOK
	}
	return nil
}

func checkDimension(ctx context.Context, tenant string, it *ReadinessItem, n *need, reg map[string]Dimension, repo Repo, on time.Time) error {
	d, ok := reg[it.Key]
	if !ok {
		it.State, it.Blocking = ReadyDimMissing, true
		it.Remedy = "POST /v1/dimensions with dimension_code " + it.Key
		return nil
	}
	if d.Status != "" && d.Status != "ACTIVE" {
		it.State, it.Blocking = ReadyDimRetired, true
		it.Remedy = "dimension " + it.Key + " is " + d.Status
		return nil
	}
	if n.reqd {
		vals, err := repo.ListDimensionValues(ctx, tenant, it.Key)
		if err != nil {
			return err
		}
		live := 0
		for _, v := range vals {
			if v.Status == "ACTIVE" && !v.ValidFrom.After(on) && (v.ValidTo == nil || on.Before(*v.ValidTo)) {
				live++
			}
		}
		if live == 0 {
			it.State, it.Blocking = ReadyDimNoValues, true
			it.Remedy = "POST /v1/dimensions/" + it.Key + "/values with at least one value valid on the date"
			return nil
		}
	}
	it.State = ReadyOK
	return nil
}

func sortedNeedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
