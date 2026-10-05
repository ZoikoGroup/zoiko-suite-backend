package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 6 (payroll): the statutory parameter interface (s17).
//
// Payroll CALCULATION stays with the payroll product. This service supplies the
// versioned, sourced, verified statutory parameters (bands, thresholds,
// allowances, contribution rates and caps, statutory pay) that apply at a pay
// date, as one consistent set with its provenance, so a payroll run can be
// reproduced from exactly what it was given.

// ParameterSetRequest asks for the PAYROLL parameters in force at an instant.
type ParameterSetRequest struct {
	Jurisdiction string
	At           time.Time
	// Classes, when set, limits the answer to those s17 classes.
	Classes     []string
	PackRef     string
	PackVersion string
}

// ParameterItem is one resolved statutory parameter with its provenance.
type ParameterItem struct {
	RuleCode      string       `json:"rule_code"`
	Class         string       `json:"class"`
	Family        string       `json:"family"`
	Parameters    rawJSON      `json:"parameters"`
	RuleID        string       `json:"rule_id"`
	RuleName      string       `json:"rule_name"`
	ContentDigest string       `json:"content_digest"`
	EffectiveFrom time.Time    `json:"effective_from"`
	EffectiveTo   *time.Time   `json:"effective_to"`
	Pack          PackInfo     `json:"pack"`
	Sources       []SourceInfo `json:"sources"`
}

// ParameterSet is the answer. Items are sorted by class then rule code.
type ParameterSet struct {
	Outcome           string          `json:"outcome"`
	Jurisdiction      string          `json:"jurisdiction"`
	EffectiveAt       time.Time       `json:"effective_at"`
	Items             []ParameterItem `json:"items"`
	AmbiguousCodes    []string        `json:"ambiguous_rule_codes,omitempty"`
	WithoutParameters []string        `json:"rules_without_parameters,omitempty"`
	BundleDigest      string          `json:"bundle_digest,omitempty"`
	PacksConsulted    []PackConsulted `json:"packs_consulted"`
	Explanation       []string        `json:"explanation"`
}

// ResolveParameterSet resolves every PAYROLL rule in force at the instant from
// verified released packs. If ANY rule code is ambiguous the outcome is
// AMBIGUOUS and the caller must not use the partial set: a payroll run must
// never silently apply some of the statutory parameters.
func (r *Resolver) ResolveParameterSet(ctx context.Context, req ParameterSetRequest) (*ParameterSet, error) {
	if strings.TrimSpace(req.Jurisdiction) == "" || req.At.IsZero() {
		return nil, fmt.Errorf("%w: jurisdiction and effective_at are required", ErrBadRequest)
	}
	if req.PackVersion != "" && req.PackRef == "" {
		return nil, fmt.Errorf("%w: pack_version needs pack_ref", ErrBadRequest)
	}
	for _, c := range req.Classes {
		if !domain.ValidParameterClass(c) {
			return nil, fmt.Errorf("%w: class %q is not one of %s", ErrBadRequest, c, strings.Join(domain.ParameterClasses, ", "))
		}
	}
	at := req.At.UTC()
	set := &ParameterSet{Jurisdiction: req.Jurisdiction, EffectiveAt: at, Items: []ParameterItem{}, PacksConsulted: []PackConsulted{}, Explanation: []string{}}
	step := func(format string, a ...any) { set.Explanation = append(set.Explanation, fmt.Sprintf(format, a...)) }

	entries, early, err := r.pick(ctx, req.Jurisdiction, req.PackRef, req.PackVersion, at, step)
	if err != nil {
		return nil, err
	}
	if early != "" {
		set.Outcome = early
		return set, nil
	}

	wantClass := map[string]bool{}
	for _, c := range req.Classes {
		wantClass[c] = true
	}
	codes := map[string]bool{}
	for _, e := range entries {
		step("pack %s@%s consulted", e.pack.PackRef, e.pack.Version)
		set.PacksConsulted = append(set.PacksConsulted, PackConsulted{PackRef: e.pack.PackRef, Version: e.pack.Version, Outcome: "CONSULTED"})
		for _, c := range e.doc.RuleCodes(domain.PayrollDomain) {
			codes[c] = true
		}
	}
	var sortedCodes []string
	for c := range codes {
		sortedCodes = append(sortedCodes, c)
	}
	sort.Strings(sortedCodes)

	for _, code := range sortedCodes {
		type hit struct {
			e   *entry
			res domain.Resolution
		}
		var resolved []hit
		ambiguous := false
		for _, e := range entries {
			res := e.doc.Resolve(req.Jurisdiction, domain.PayrollDomain, code, at)
			switch res.Outcome {
			case domain.OutcomeResolved:
				resolved = append(resolved, hit{e, res})
			case domain.OutcomeAmbiguous:
				ambiguous = true
			}
		}
		if ambiguous || len(resolved) > 1 {
			set.AmbiguousCodes = append(set.AmbiguousCodes, code)
			step("rule %s is ambiguous (overlap without declared precedence, or defined by more than one pack): blocked", code)
			continue
		}
		if len(resolved) == 0 {
			continue // not in force at this instant
		}
		h := resolved[0]
		var rule *domain.ArtifactRule
		for i := range h.e.doc.Rules {
			if h.e.doc.Rules[i].RuleID == h.res.RuleID {
				rule = &h.e.doc.Rules[i]
			}
		}
		if rule == nil {
			continue
		}
		if len(rule.Parameters) == 0 {
			set.WithoutParameters = append(set.WithoutParameters, code)
			step("rule %s is in force but carries no statutory parameters", code)
			continue
		}
		p, perr := domain.ParseRuleParameters(rule.Parameters)
		if perr != nil {
			set.AmbiguousCodes = append(set.AmbiguousCodes, code) // an unusable parameter set blocks the answer like a conflict
			step("rule %s carries invalid parameters: %v", code, perr)
			continue
		}
		if len(wantClass) > 0 && !wantClass[p.Class] {
			continue
		}
		item := ParameterItem{RuleCode: code, Class: p.Class, Family: p.Family, Parameters: rawJSON(rule.Parameters), RuleID: rule.RuleID, RuleName: rule.RuleName,
			ContentDigest: rule.ContentDigest, EffectiveFrom: rule.EffectiveFrom, EffectiveTo: rule.EffectiveTo, Pack: h.e.pack, Sources: []SourceInfo{}}
		for _, sid := range rule.SourceIDs {
			if s, ok := h.e.doc.Sources[sid]; ok {
				item.Sources = append(item.Sources, SourceInfo{SourceID: s.SourceID, Authority: s.Authority, SourceType: s.SourceType,
					AuthorityLevel: s.AuthorityLevel, Title: s.Title, SnapshotHash: s.SnapshotHash})
			}
		}
		sort.Slice(item.Sources, func(a, b int) bool { return item.Sources[a].SourceID < item.Sources[b].SourceID })
		set.Items = append(set.Items, item)
	}
	sort.SliceStable(set.Items, func(a, b int) bool {
		if set.Items[a].Class != set.Items[b].Class {
			return set.Items[a].Class < set.Items[b].Class
		}
		return set.Items[a].RuleCode < set.Items[b].RuleCode
	})

	switch {
	case len(set.AmbiguousCodes) > 0:
		set.Outcome = domain.OutcomeAmbiguous
		set.Items = []ParameterItem{} // never hand out a partial set
	case len(set.Items) == 0:
		set.Outcome = domain.OutcomeNoRule
		step("no payroll statutory parameter is in force for %s at %s", req.Jurisdiction, at.Format(time.RFC3339))
	default:
		set.Outcome = domain.OutcomeParametersResolved
		raw, _ := json.Marshal(set.Items)
		set.BundleDigest, _ = domain.DigestOf(raw)
		step("%d statutory parameter(s) resolved; bundle %s", len(set.Items), set.BundleDigest)
	}
	return set, nil
}
