// COM-03 Entitlement (ZS-SVC-Q-001 §4.3). The sole runtime decision for
// whether a tenant may use a commercial capability and at what limit.
//
// COM-03 answers commercial purchase eligibility only — "is this feature/
// limit included in the customer's effective commercial package" — never
// identity permission (GOV-03/04) or domain validity, which the caller must
// still check independently (§4.3 Commercial vs authorization decision).
package domain

import "time"

const PrefixRestriction = "crst_"

// Outcome is the decision COM-03 returns. Ranked from least to most severe;
// a restriction can only move a decision toward Deny, never away from it.
type Outcome string

const (
	OutcomeAllow          Outcome = "ALLOW"
	OutcomeAllowWithLimit Outcome = "ALLOW_WITH_LIMIT"
	OutcomeReadOnly       Outcome = "READ_ONLY"
	OutcomeRestricted     Outcome = "RESTRICTED"
	OutcomeDeny           Outcome = "DENY"
)

var outcomeSeverity = map[Outcome]int{
	OutcomeAllow: 0, OutcomeAllowWithLimit: 1, OutcomeReadOnly: 2, OutcomeRestricted: 3, OutcomeDeny: 4,
}

// mostSevere returns whichever outcome is closer to Deny. It is the whole of
// how a restriction combines with a subscription-based result, and how
// several active restrictions combine with each other: never a vote, never
// an average, always the strictest fact standing.
func mostSevere(a, b Outcome) Outcome {
	if outcomeSeverity[b] > outcomeSeverity[a] {
		return b
	}
	return a
}

// RestrictionLevel is a commercial restriction's own severity, distinct from
// Outcome because a restriction never grants; ALLOW is not a valid level.
type RestrictionLevel string

const (
	RestrictionReadOnly   RestrictionLevel = "READ_ONLY"
	RestrictionRestricted RestrictionLevel = "RESTRICTED"
)

func (l RestrictionLevel) outcome() Outcome { return Outcome(l) }

// CommercialRestriction lowers what an organization may do, for a reason,
// under a named policy — a dunning case, a legal hold. It is applied once
// and lifted once; it can never itself be a grant.
type CommercialRestriction struct {
	RestrictionID        string           `json:"restriction_id"`
	OrganizationID       string           `json:"organization_id"`
	Level                RestrictionLevel `json:"level"`
	ReasonCode           string           `json:"reason_code"`
	PolicyRef            string           `json:"policy_ref"`
	BasisRef             string           `json:"basis_ref"`
	AppliedAt            time.Time        `json:"applied_at"`
	AppliedByPrincipalID string           `json:"applied_by_principal_id"`
	LiftedAt             *time.Time       `json:"lifted_at,omitempty"`
	LiftedByPrincipalID  *string          `json:"lifted_by_principal_id,omitempty"`
	LiftReason           *string          `json:"lift_reason,omitempty"`
}

func (r CommercialRestriction) activeAt(t time.Time) bool {
	return !r.AppliedAt.After(t) && (r.LiftedAt == nil || r.LiftedAt.After(t))
}

// EntitlementPolicyVersion is the versioned, effective-dated rule for what
// an ended subscription still allows (COM-CTRL-012). No version means an
// ended subscription is denied — never assumed read-only.
type EntitlementPolicyVersion struct {
	PolicyVersion        int       `json:"policy_version"`
	EndedOutcome         string    `json:"ended_outcome"`
	EndedReadOnlyDays    int       `json:"ended_read_only_days"`
	EffectiveFrom        time.Time `json:"effective_from"`
	Reason               string    `json:"reason"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// SubscriptionEntitlementBasis is everything a decision needs about the
// organization's subscription at the instant being evaluated: its status,
// how long ago it ended (if it has), and the capabilities every price
// version it is bound to (plan and add-ons) together define. A capability
// key absent from every bound version is not included — DENY, never assumed.
type SubscriptionEntitlementBasis struct {
	SubscriptionID string
	Status         LifecycleStatus // zero value ("") means no subscription exists at all
	EndedAt        *time.Time      // set once Status is Canceled/Expired
	Capabilities   map[string]PlanCapability
}

// CapabilityDecision is EvaluateCapability's result, and ExplainDecision's
// basis: enough to answer both "what" and "why".
type CapabilityDecision struct {
	CapabilityKey        string    `json:"capability_key"`
	Outcome              Outcome   `json:"outcome"`
	LimitValue           *int64    `json:"limit_value,omitempty"`
	LimitUnit            *string   `json:"limit_unit,omitempty"`
	SubscriptionOutcome  Outcome   `json:"subscription_outcome"`
	RestrictionOutcome   *Outcome  `json:"restriction_outcome,omitempty"`
	AppliedRestrictionID *string   `json:"applied_restriction_id,omitempty"`
	PolicyVersion        *int      `json:"policy_version,omitempty"`
	SubscriptionID       *string   `json:"subscription_id,omitempty"`
	Reason               string    `json:"reason"`
	DecidedAt            time.Time `json:"decided_at"`
}

// EvaluateCapability is COM-03's one decision function. Every other query
// (GetEffectiveEntitlements, GetLimit, ExplainDecision) is this function
// called differently, so there is exactly one place the rule lives.
//
// requestedQuantity is optional: nil answers "is this capability included
// and at what limit"; a value additionally checks that quantity against the
// limit (over-limit denies — COM-03 does not know current consumption, that
// is COM-04's usage ledger once it exists; this only catches a single
// request that alone exceeds the whole limit).
func EvaluateCapability(capabilityKey string, basis SubscriptionEntitlementBasis, restrictions []CommercialRestriction,
	policy *EntitlementPolicyVersion, requestedQuantity *int64, now time.Time) CapabilityDecision {
	d := CapabilityDecision{CapabilityKey: capabilityKey, DecidedAt: now}
	if basis.SubscriptionID != "" {
		d.SubscriptionID = &basis.SubscriptionID
	}

	switch {
	case basis.Status == "":
		d.SubscriptionOutcome, d.Reason = OutcomeDeny, "no subscription exists for this organization"
	case basis.Status.Terminal():
		d.SubscriptionOutcome, d.Reason = endedOutcome(basis, policy, now, &d)
	case basis.Status == LifecyclePending:
		d.SubscriptionOutcome, d.Reason = OutcomeDeny, "subscription is PENDING and has not yet been activated"
	default: // Trialing, Active, CancelPending: still in service.
		cap, ok := basis.Capabilities[capabilityKey]
		if !ok {
			d.SubscriptionOutcome, d.Reason = OutcomeDeny, "capability is not included in the effective subscription"
			break
		}
		d.LimitValue, d.LimitUnit = cap.LimitValue, cap.LimitUnit
		switch {
		case cap.LimitValue == nil:
			d.SubscriptionOutcome, d.Reason = OutcomeAllow, "capability is included, unlimited"
		case requestedQuantity != nil && *requestedQuantity > *cap.LimitValue:
			d.SubscriptionOutcome, d.Reason = OutcomeDeny, "requested quantity exceeds the effective limit"
		default:
			d.SubscriptionOutcome, d.Reason = OutcomeAllowWithLimit, "capability is included, subject to its limit"
		}
	}

	d.Outcome = d.SubscriptionOutcome
	for i, r := range restrictions {
		if !r.activeAt(now) {
			continue
		}
		ro := r.Level.outcome()
		if d.RestrictionOutcome == nil || outcomeSeverity[ro] > outcomeSeverity[*d.RestrictionOutcome] {
			d.RestrictionOutcome = &ro
			d.AppliedRestrictionID = &restrictions[i].RestrictionID
		}
	}
	if d.RestrictionOutcome != nil {
		combined := mostSevere(d.Outcome, *d.RestrictionOutcome)
		if combined != d.Outcome {
			d.Outcome = combined
			d.Reason = "reduced by an active commercial restriction"
		}
	}
	return d
}

func endedOutcome(basis SubscriptionEntitlementBasis, policy *EntitlementPolicyVersion, now time.Time, d *CapabilityDecision) (Outcome, string) {
	if policy == nil {
		return OutcomeDeny, "subscription has ended and no ended-access policy has been published"
	}
	d.PolicyVersion = &policy.PolicyVersion
	if policy.EndedOutcome == "DENY" || basis.EndedAt == nil {
		return OutcomeDeny, "subscription has ended; the effective policy grants no further access"
	}
	graceEnds := basis.EndedAt.Add(time.Duration(policy.EndedReadOnlyDays) * 24 * time.Hour)
	if now.Before(graceEnds) {
		return OutcomeReadOnly, "subscription has ended; within its read-only grace period"
	}
	return OutcomeDeny, "subscription's read-only grace period has passed"
}

// EffectiveEntitlements evaluates every capability the subscription's bound
// price versions define, for GetEffectiveEntitlements.
func EffectiveEntitlements(basis SubscriptionEntitlementBasis, restrictions []CommercialRestriction,
	policy *EntitlementPolicyVersion, now time.Time) []CapabilityDecision {
	if len(basis.Capabilities) == 0 {
		// Still worth one decision: DENY with the actual reason (no
		// subscription, ended, pending), not an empty, unexplained list.
		return []CapabilityDecision{EvaluateCapability("", basis, restrictions, policy, nil, now)}
	}
	keys := make([]string, 0, len(basis.Capabilities))
	for k := range basis.Capabilities {
		keys = append(keys, k)
	}
	out := make([]CapabilityDecision, len(keys))
	for i, k := range keys {
		out[i] = EvaluateCapability(k, basis, restrictions, policy, nil, now)
	}
	return out
}

var (
	ErrRestrictionNotFound      = errorString("restriction not found")
	ErrRestrictionAlreadyLifted = errorString("restriction is already lifted")
)
