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

	CreatedAt             time.Time `json:"created_at"`
	CreatedByPrincipalID  string    `json:"created_by_principal_id"`
	SchemaVersion         string    `json:"schema_version"`
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
	// Current value only — full transition history is in drift_events table.
	LegalDriftState string `json:"legal_drift_state"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	SchemaVersion        string    `json:"schema_version"`
}

// ErrJurisdictionNotFound is returned when the jurisdiction_id does not exist
// or is inactive. Callers (e.g. tenant-entity-registry-svc) must reject the
// assignment fail-closed when they receive this error.
var ErrJurisdictionNotFound = errorString("jurisdiction not found")

// ErrStoreUnavailable is returned when the database cannot be reached.
// Callers must fail-closed — treat as unavailable, not as "not found".
var ErrStoreUnavailable = errorString("jurisdiction rules store unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }
