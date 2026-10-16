// Package resolve decides which policy version governs a request at a given
// business time (as_of) and knowledge time (known_at) — GOV-05
// ResolveEffectivePolicy / ExplainPolicyResolution and V-001 §8 and §12.2.
//
// It is a pure function of the candidate versions it is handed, so the same
// rules serve live evaluation, cached evaluation and historical replay.
//
// The rules, per (policy, scope) chain:
//
//  1. A version is known from the moment its activation (or scheduled
//     activation) was recorded — activated_at — and only from then.
//  2. Of the known versions whose effective_from is at or before as_of, the
//     most recently recorded one governs. A successor therefore takes over
//     from its own effective_from, and its predecessor keeps governing the
//     time before that: history is never rewritten.
//  3. If that version was suspended or retired at or before known_at, the
//     chain yields nothing. The predecessor is NOT resurrected: V-001
//     PDC-I-28 forbids an emergency fallback without a governed exception.
//     A version withdrawn before it ever went live (a cancelled schedule)
//     is ignored instead, so the version it would have replaced stays.
//  4. If that version's effective_to is at or before as_of, the chain yields
//     nothing (no fallback to an older version).
//
// Across chains, the most specific scope wins (legal entity > tenant >
// global). Two distinct policies at the same, most specific, tier is
// CONFLICTED — never "latest by timestamp" (GOV-05 failure semantics,
// V-001 PDC-I-08).
package resolve

import (
	"sort"
	"time"

	"zoiko.io/policy-svc/internal/domain"
)

// Status is the applicability state (V-001 §5 Applicability).
type Status string

const (
	Resolved      Status = "RESOLVED"
	NotApplicable Status = "NOT_APPLICABLE"
	Conflicted    Status = "CONFLICTED"
)

// Reasons a candidate did not govern. They appear in the explain trace.
const (
	ReasonSelected              = "selected"
	ReasonNeverActivated        = "never_activated"
	ReasonActivatedAfterKnownAt = "activated_after_known_at"
	ReasonCancelledBeforeLive   = "cancelled_before_effective"
	ReasonNotYetEffective       = "not_yet_effective"
	ReasonSupersededInChain     = "superseded_by_later_version"
	ReasonWithdrawn             = "suspended_or_retired"
	ReasonExpired               = "effective_to_passed"
	ReasonLessSpecific          = "overridden_by_more_specific_scope"
	ReasonConflicting           = "conflicts_with_another_policy"
)

// TraceEntry explains what happened to one candidate.
type TraceEntry struct {
	PolicyVersionID string `json:"policy_version_id"`
	PolicyID        string `json:"policy_id"`
	PolicyCode      string `json:"policy_code"`
	VersionNumber   int    `json:"version_number"`
	ScopeType       string `json:"scope_type"`
	VersionStatus   string `json:"version_status"`
	Governs         bool   `json:"governs"`
	Reason          string `json:"reason"`
}

// Result is the outcome of one resolution.
type Result struct {
	Status  Status    `json:"status"`
	AsOf    time.Time `json:"as_of"`
	KnownAt time.Time `json:"known_at"`

	// Selected is set only when Status is RESOLVED.
	Selected *domain.ApplicablePolicyVersion `json:"selected,omitempty"`

	// Conflicting lists the tied versions when Status is CONFLICTED.
	Conflicting []*domain.ApplicablePolicyVersion `json:"conflicting,omitempty"`

	// Applicable is the governing version of every chain, most specific
	// scope first — the "applicable policy set" of Doc 03 §8.1.
	Applicable []*domain.ApplicablePolicyVersion `json:"-"`

	// Trace has one entry per candidate, in input order.
	Trace []TraceEntry `json:"trace"`
}

// specificity ranks a scope: legal entity 2, tenant 1, global 0.
func specificity(v *domain.ApplicablePolicyVersion) int {
	n := 0
	if v.TenantID != nil {
		n++
	}
	if v.LegalEntityID != nil {
		n++
	}
	return n
}

func chainKey(v *domain.ApplicablePolicyVersion) string {
	return v.PolicyID + "|" + deref(v.TenantID) + "|" + deref(v.LegalEntityID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// withdrawnBy reports whether v was suspended or retired at or before knownAt.
func withdrawnBy(v *domain.ApplicablePolicyVersion, knownAt time.Time) bool {
	return v.WithdrawnAt != nil && !v.WithdrawnAt.After(knownAt)
}

// cancelledBeforeLive reports whether v was withdrawn (as known at knownAt)
// before it ever started to govern: a scheduled activation that was called
// off. Such a version never displaced anything.
func cancelledBeforeLive(v *domain.ApplicablePolicyVersion, knownAt time.Time) bool {
	if !withdrawnBy(v, knownAt) {
		return false
	}
	live := v.EffectiveFrom
	if v.ActivatedAt != nil && v.ActivatedAt.After(live) {
		live = *v.ActivatedAt
	}
	return v.WithdrawnAt.Before(live)
}

// later reports whether a was recorded after b (ties: higher version number).
func later(a, b *domain.ApplicablePolicyVersion) bool {
	if !a.ActivatedAt.Equal(*b.ActivatedAt) {
		return a.ActivatedAt.After(*b.ActivatedAt)
	}
	return a.VersionNumber > b.VersionNumber
}

// Resolve applies the package rules to candidates. Candidates must already be
// limited to one policy type and to scopes compatible with the request (their
// tenant and legal entity are either unset or equal to the request's).
func Resolve(candidates []*domain.ApplicablePolicyVersion, asOf, knownAt time.Time) Result {
	res := Result{Status: NotApplicable, AsOf: asOf, KnownAt: knownAt}
	reasons := make(map[*domain.ApplicablePolicyVersion]string, len(candidates))

	// Rule 1 and the cancelled-schedule carve-out of rule 3.
	chains := map[string][]*domain.ApplicablePolicyVersion{}
	for _, v := range candidates {
		switch {
		case v.ActivatedAt == nil:
			reasons[v] = ReasonNeverActivated
		case v.ActivatedAt.After(knownAt):
			reasons[v] = ReasonActivatedAfterKnownAt
		case cancelledBeforeLive(v, knownAt):
			reasons[v] = ReasonCancelledBeforeLive
		case v.EffectiveFrom.After(asOf):
			reasons[v] = ReasonNotYetEffective
		default:
			chains[chainKey(v)] = append(chains[chainKey(v)], v)
		}
	}

	// Rules 2-4: one governing version per chain, at most.
	var winners []*domain.ApplicablePolicyVersion
	for _, chain := range chains {
		head := chain[0]
		for _, v := range chain[1:] {
			if later(v, head) {
				head = v
			}
		}
		for _, v := range chain {
			if v != head {
				reasons[v] = ReasonSupersededInChain
			}
		}
		switch {
		case withdrawnBy(head, knownAt):
			reasons[head] = ReasonWithdrawn
		case head.EffectiveTo != nil && !head.EffectiveTo.After(asOf):
			reasons[head] = ReasonExpired
		default:
			winners = append(winners, head)
		}
	}

	// Most specific tier; a tie between distinct policies is a conflict.
	if len(winners) > 0 {
		sort.SliceStable(winners, func(i, j int) bool {
			if specificity(winners[i]) != specificity(winners[j]) {
				return specificity(winners[i]) > specificity(winners[j])
			}
			return winners[i].PolicyVersionID < winners[j].PolicyVersionID
		})
		res.Applicable = winners
		top := specificity(winners[0])
		var tied []*domain.ApplicablePolicyVersion
		for _, w := range winners {
			if specificity(w) == top {
				tied = append(tied, w)
			} else {
				reasons[w] = ReasonLessSpecific
			}
		}
		if len(tied) == 1 {
			res.Status = Resolved
			res.Selected = tied[0]
			reasons[tied[0]] = ReasonSelected
		} else {
			res.Status = Conflicted
			sort.Slice(tied, func(i, j int) bool { return tied[i].PolicyVersionID < tied[j].PolicyVersionID })
			res.Conflicting = tied
			for _, t := range tied {
				reasons[t] = ReasonConflicting
			}
		}
	}

	res.Trace = make([]TraceEntry, 0, len(candidates))
	for _, v := range candidates {
		res.Trace = append(res.Trace, TraceEntry{
			PolicyVersionID: v.PolicyVersionID,
			PolicyID:        v.PolicyID,
			PolicyCode:      v.PolicyCode,
			VersionNumber:   v.VersionNumber,
			ScopeType:       v.ScopeType,
			VersionStatus:   v.VersionStatus,
			Governs:         res.Selected == v,
			Reason:          reasons[v],
		})
	}
	return res
}

// Times picks the business and knowledge time for a request. With neither
// given, both are now. A past as_of alone defaults known_at to the same
// instant — "what governed then, as the platform knew it then", the
// historical-request negative path of GOV-05 — while a future as_of is
// answered with what is known now. known_at may never be in the future: the
// platform cannot know what it has not yet recorded.
func Times(asOf, knownAt *time.Time, now time.Time) (time.Time, time.Time, bool) {
	a := now
	if asOf != nil {
		a = *asOf
	}
	k := now
	switch {
	case knownAt != nil:
		if knownAt.After(now) {
			return time.Time{}, time.Time{}, false
		}
		k = *knownAt
	case asOf != nil && asOf.Before(now):
		k = *asOf
	}
	return a, k, true
}
