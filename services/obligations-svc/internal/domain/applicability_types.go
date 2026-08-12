// Applicability decision domain types — doc7 §E2.
//
// decision is a plain string, DATA ONLY — same doctrine as
// obligation_status elsewhere in this package. UNASSESSED is never stored
// as a row; it is what FindCurrentApplicability returns when no decision
// row exists at all for a scope, so "never assessed" can never be confused
// with "assessed and found not applicable" — doc7 §E2's own explicit
// warning.
package domain

import (
	"encoding/json"
	"time"
)

// ApplicabilityDecision is doc7 §E2's versioned applicability decision: a
// single point-in-time conclusion about whether an obligation applies in a
// given jurisdiction/entity/activity/product scope, with the source rule,
// facts, and actor/system that produced it. Append-only — a new decision is
// always a new row, never an update of a prior one.
type ApplicabilityDecision struct {
	ApplicabilityDecisionID string `json:"applicability_decision_id"`
	ObligationID            string `json:"obligation_id"`

	JurisdictionCode  string  `json:"jurisdiction_code"`
	EntityRef         string  `json:"entity_ref"`
	ActivityRef       *string `json:"activity_ref,omitempty"`
	ProductProcessRef *string `json:"product_process_ref,omitempty"`

	// Decision: APPLICABLE | NOT_APPLICABLE | UNCERTAIN. See package doc —
	// UNASSESSED is never a value stored here.
	Decision string `json:"decision"`

	SourceRuleRef     string `json:"source_rule_ref"`
	SourceRuleVersion string `json:"source_rule_version"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`

	FactsUsed json.RawMessage `json:"facts_used"`

	Confidence       *float64 `json:"confidence,omitempty"`
	UncertaintyNotes *string  `json:"uncertainty_notes,omitempty"`

	// Exactly one of these two is set — the actor/system that reached this
	// decision (doc7 §E2's "actor/system").
	DecidedByPrincipalID *string `json:"decided_by_principal_id,omitempty"`
	DecidedBySystem      *string `json:"decided_by_system,omitempty"`

	ReviewRequired bool `json:"review_required"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// CreateApplicabilityDecisionParams holds input for recording a new
// applicability decision.
type CreateApplicabilityDecisionParams struct {
	ApplicabilityDecisionID string
	ObligationID            string
	JurisdictionCode        string
	EntityRef               string
	ActivityRef             string
	ProductProcessRef       string
	Decision                string
	SourceRuleRef           string
	SourceRuleVersion       string
	EffectiveFrom           time.Time
	EffectiveTo             *time.Time
	FactsUsed               json.RawMessage
	Confidence              *float64
	UncertaintyNotes        string
	DecidedByPrincipalID    string
	DecidedBySystem         string
	ReviewRequired          bool
	CreatedByPrincipalID    string
}

// CurrentApplicability is the answer to "is this obligation applicable in
// this scope right now" — composing the presence/absence of a decision row
// with its content. Status is UNASSESSED when no decision row exists for
// the scope at all; this is a DIFFERENT value from a stored NOT_APPLICABLE
// decision, per doc7 §E2's explicit requirement that unknown applicability
// never defaults to not-applicable.
type CurrentApplicability struct {
	ObligationID     string                 `json:"obligation_id"`
	JurisdictionCode string                 `json:"jurisdiction_code"`
	EntityRef        string                 `json:"entity_ref"`
	Status           string                 `json:"status"` // UNASSESSED | APPLICABLE | NOT_APPLICABLE | UNCERTAIN
	Decision         *ApplicabilityDecision `json:"decision,omitempty"`
}

// ErrApplicabilityDecisionInvalid is returned when a decision request is
// missing both actor and system, or fails another domain-level validation.
var ErrApplicabilityDecisionInvalid = errorString("applicability decision requires either decided_by_principal_id or decided_by_system")
