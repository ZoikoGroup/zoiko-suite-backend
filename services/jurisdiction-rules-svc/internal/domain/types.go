// Package domain contains the authoritative domain types for jurisdiction-rules-svc.
//
// All type/status/domain discriminator fields are plain strings — no Go enums,
// iota, or switch/case branches. New jurisdiction types, rule domains, or
// status values are added via data migration only; no code changes required
// (per .agents/rules/doctrine.md and OQ-3 approval).
package domain

import (
	"encoding/json"
	"time"
)

// Jurisdiction is the authoritative registry entry for a jurisdiction.
// Jurisdictions nest via ParentJurisdictionID (country → state → tax authority).
// No soft-delete: deactivation uses ActiveFlag + EffectiveTo.
type Jurisdiction struct {
	JurisdictionID string `json:"jurisdiction_id"`

	// JurisdictionCode is a human-readable short code e.g. "GB", "US-CA".
	// It is DATA ONLY — never used as a code switch/case branch.
	JurisdictionCode string `json:"jurisdiction_code"`

	JurisdictionName string `json:"jurisdiction_name"`

	// JurisdictionType is a VARCHAR tag stored as data:
	// e.g. COUNTRY, STATE_PROVINCE, TAX_AUTHORITY, LABOR_LAW_BOUNDARY,
	//      FILING_AUTHORITY, DATA_RESIDENCY_BOUNDARY.
	// New types require a data migration only, no code change.
	JurisdictionType string `json:"jurisdiction_type"`

	// ParentJurisdictionID is nil for root jurisdictions.
	ParentJurisdictionID *string `json:"parent_jurisdiction_id"`

	// AuthorityType: FEDERAL, STATE, MUNICIPAL, SUPRANATIONAL — data driven.
	AuthorityType string `json:"authority_type"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	ActiveFlag bool `json:"active_flag"`

	// DataClassification is PUBLIC for jurisdictions
	// (data_classification_audit.md §2.11) — names, region codes, country codes.
	DataClassification string `json:"data_classification"`

	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	SchemaVersion        string     `json:"schema_version"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
	UpdatedByPrincipalID *string    `json:"updated_by_principal_id,omitempty"`
}

// JurisdictionRule is an effective-dated applicability rule record.
//
// rule_payload holds APPLICABILITY METADATA ONLY — not computation values.
// Thresholds, rates, and bands belong to Tax/Payroll services (OQ-1, Model B).
//
// Example payload:
//
//	{"applies_to_entity_types": ["COMPANY","BRANCH"],
//	 "filing_frequency": "MONTHLY",
//	 "authority_code": "HMRC"}
type JurisdictionRule struct {
	JurisdictionRuleID string `json:"jurisdiction_rule_id"`
	JurisdictionID     string `json:"jurisdiction_id"`

	// RuleDomain: PAYROLL, TAX, EMPLOYMENT, FILING, RETENTION, BENEFITS.
	// VARCHAR — extensible via data only, never a code switch.
	RuleDomain string `json:"rule_domain"`

	RuleCode string `json:"rule_code"`
	RuleName string `json:"rule_name"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	// RulePayload — applicability metadata ONLY (OQ-1 Model B).
	// json.RawMessage so the payload is inlined in API responses as JSON,
	// not base64-encoded bytes. Callers decode against schema_version.
	RulePayload json.RawMessage `json:"rule_payload"`

	SourceReference       *string `json:"source_reference"`
	ExternalFeedReference *string `json:"external_feed_reference"`

	// RuleStatus: ACTIVE, SUPERSEDED, DRAFT, RETIRED — VARCHAR, not enum.
	RuleStatus string `json:"rule_status"`

	// LegalDriftState: CURRENT, DRIFTED, UNDER_REVIEW.
	// Current value only — full transition history is in drift_events table,
	// readable via GET /v1/rules/{id}/drift-events.
	LegalDriftState string `json:"legal_drift_state"`

	// DataClassification is INTERNAL for rules
	// (data_classification_audit.md §2.11) — rule domain settings and
	// legislative metadata.
	DataClassification string `json:"data_classification"`

	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	SchemaVersion        string     `json:"schema_version"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
	UpdatedByPrincipalID *string    `json:"updated_by_principal_id,omitempty"`
}

// DriftEvent is one append-only entry in the legal_drift_state transition
// history (OQ-4). jurisdiction_rules.legal_drift_state carries only the
// current value; this is the record of how it got there, which is what
// "preserve historical rule state for replay and audit" requires.
type DriftEvent struct {
	DriftEventID          string    `json:"drift_event_id"`
	JurisdictionRuleID    string    `json:"jurisdiction_rule_id"`
	FromState             string    `json:"from_state"`
	ToState               string    `json:"to_state"`
	Reason                *string   `json:"reason"`
	EffectiveAt           time.Time `json:"effective_at"`
	RecordedByPrincipalID string    `json:"recorded_by_principal_id"`
	CorrelationID         *string   `json:"correlation_id"`
	SchemaVersion         string    `json:"schema_version"`
}

// RecordDriftParams holds input parameters for recording a drift transition.
type RecordDriftParams struct {
	JurisdictionRuleID    string
	ToState               string
	Reason                *string
	RecordedByPrincipalID string
	CorrelationID         string
}

// RulePack is the resolved, runtime-ready rule set for a jurisdiction at a
// point in time — the "fetch runtime rule pack" / "resolve jurisdiction set"
// capability of 03-microservices.md §8.2.
//
// Rules are collected from the jurisdiction itself and every ancestor, then
// narrowed so that exactly one rule wins per (rule_domain, rule_code): the
// most specific jurisdiction wins, and within one jurisdiction the latest
// effective_from wins. ResolvedFrom records which jurisdictions contributed,
// nearest first, so a caller can explain the basis of a governed action.
type RulePack struct {
	JurisdictionID string    `json:"jurisdiction_id"`
	EffectiveAt    time.Time `json:"effective_at"`

	// ResolvedFrom is the jurisdiction chain the pack was assembled from,
	// self first then ancestors outward to the root.
	ResolvedFrom []string `json:"resolved_from"`

	Rules []*JurisdictionRule `json:"rules"`
}

// CreateJurisdictionParams holds input parameters for creating a jurisdiction.
type CreateJurisdictionParams struct {
	JurisdictionID       string     `json:"jurisdiction_id"`
	JurisdictionCode     string     `json:"jurisdiction_code"`
	JurisdictionName     string     `json:"jurisdiction_name"`
	JurisdictionType     string     `json:"jurisdiction_type"`
	ParentJurisdictionID *string    `json:"parent_jurisdiction_id"`
	AuthorityType        string     `json:"authority_type"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to"`
	ActiveFlag           bool       `json:"active_flag"`
	DataClassification   string     `json:"data_classification"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	SchemaVersion        string     `json:"schema_version"`
}

// CreateRuleParams holds input parameters for creating a rule.
type CreateRuleParams struct {
	JurisdictionRuleID    string     `json:"jurisdiction_rule_id"`
	JurisdictionID        string     `json:"jurisdiction_id"`
	RuleDomain            string     `json:"rule_domain"`
	RuleCode              string     `json:"rule_code"`
	RuleName              string     `json:"rule_name"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to"`
	RulePayload           []byte     `json:"rule_payload"`
	SourceReference       *string    `json:"source_reference"`
	ExternalFeedReference *string    `json:"external_feed_reference"`
	RuleStatus            string     `json:"rule_status"`
	LegalDriftState       string     `json:"legal_drift_state"`
	DataClassification    string     `json:"data_classification"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	SchemaVersion         string     `json:"schema_version"`
}

// ErrJurisdictionNotFound is returned when the jurisdiction_id does not exist
// or is inactive. Callers (e.g. tenant-entity-registry-svc) must reject the
// assignment fail-closed when they receive this error.
var ErrJurisdictionNotFound = errorString("jurisdiction not found")

// ErrRuleNotFound is returned when a jurisdiction rule does not exist.
var ErrRuleNotFound = errorString("jurisdiction rule not found")

// ErrParentNotFound is returned when parent_jurisdiction_id references a
// jurisdiction that does not exist. Distinguished from
// ErrJurisdictionNotFound so the caller learns which id was bad — without
// it the foreign-key violation surfaced as a 503 that read like an outage.
var ErrParentNotFound = errorString("parent jurisdiction not found")

// ErrCyclicHierarchy is returned when a parent assignment would make a
// jurisdiction its own ancestor. The self-referential FK cannot express
// this, so it is enforced in the store.
var ErrCyclicHierarchy = errorString("parent assignment would create a cycle in the jurisdiction hierarchy")

// ErrOverlappingRule is returned when a new rule's effective period overlaps
// an existing non-retired rule with the same (jurisdiction_id, rule_domain,
// rule_code). Two rules matching the same point-in-time query make "the
// effective rule at date X" ambiguous, which the effective-dating model in
// 04-data-model.md §1020 does not permit.
var ErrOverlappingRule = errorString("rule effective period overlaps an existing rule for the same rule_code")

// ErrInvalidTransition is returned when a rule status transition is illegal per state machine.
var ErrInvalidTransition = errorString("invalid rule status transition")

// ErrConflict is returned when an idempotent creation request matches an existing record's dedup key
// but has differing payload or attributes (409 Conflict).
var ErrConflict = errorString("conflict: record already exists with differing attributes")

// ErrInvalidEffectivePeriod is returned when effective_to is not strictly
// after effective_from. A zero-length or inverted period can never be
// returned by a point-in-time query, so accepting one silently creates a
// rule that exists but is unreachable.
var ErrInvalidEffectivePeriod = errorString("effective_to must be after effective_from")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("jurisdiction rules store unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
