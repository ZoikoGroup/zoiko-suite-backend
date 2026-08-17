// Package domain defines the authoritative domain types for
// retention-registry-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §J1-J3: this service answers
// exactly one question for every other service that owns deletable data —
// "is it safe to delete/export/migrate this right now?" It never deletes
// anything itself. record_class/policy_status/hold_status are all plain
// strings, DATA ONLY, no code switch/case in this service.
package domain

import "time"

// RetentionPolicy is a versioned rule for how long a record class may be
// kept. Immutable once created — a changed policy is a new row, never an
// UPDATE, same doctrine as policy-svc's PolicyVersion.
type RetentionPolicy struct {
	RetentionPolicyID    string     `json:"retention_policy_id"`
	RecordClass          string     `json:"record_class"`
	JurisdictionCode     *string    `json:"jurisdiction_code,omitempty"`
	TenantID             *string    `json:"tenant_id,omitempty"`
	MinRetentionDays     int        `json:"min_retention_days"`
	MaxRetentionDays     *int       `json:"max_retention_days,omitempty"`
	LegalRegulatoryBasis string     `json:"legal_regulatory_basis"`
	SourceRightsBasis    *string    `json:"source_rights_basis,omitempty"`
	PrivacyBasis         *string    `json:"privacy_basis,omitempty"`
	PolicyStatus         string     `json:"policy_status"` // ACTIVE | SUPERSEDED | RETIRED
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// CreateRetentionPolicyRequest is the wire shape for
// POST /v1/retention-policies.
type CreateRetentionPolicyRequest struct {
	RecordClass          string `json:"record_class"`
	JurisdictionCode     string `json:"jurisdiction_code,omitempty"`
	TenantID             string `json:"tenant_id,omitempty"`
	MinRetentionDays     int    `json:"min_retention_days"`
	MaxRetentionDays     *int   `json:"max_retention_days,omitempty"`
	LegalRegulatoryBasis string `json:"legal_regulatory_basis"`
	SourceRightsBasis    string `json:"source_rights_basis,omitempty"`
	PrivacyBasis         string `json:"privacy_basis,omitempty"`
	EffectiveFrom        string `json:"effective_from"`
	CorrelationID        string `json:"correlation_id"`
}

// LegalHold is doc7 §J3's freeze override — blocks deletion/redaction/
// export/migration for its scope until released, regardless of what any
// RetentionPolicy would otherwise permit.
type LegalHold struct {
	LegalHoldID                  string     `json:"legal_hold_id"`
	ScopeDescription             string     `json:"scope_description"`
	CustodiansObjects            []string   `json:"custodians_objects"`
	Authority                    string     `json:"authority"`
	RecordClass                  *string    `json:"record_class,omitempty"`
	TenantID                     *string    `json:"tenant_id,omitempty"`
	EntityRef                    *string    `json:"entity_ref,omitempty"`
	HoldStatus                   string     `json:"hold_status"` // ACTIVE | RELEASED
	StartedAt                    time.Time  `json:"started_at"`
	ReleasedAt                   *time.Time `json:"released_at,omitempty"`
	ReleasedByPrincipalID        *string    `json:"released_by_principal_id,omitempty"`
	ReleaseApprovedByPrincipalID *string    `json:"release_approved_by_principal_id,omitempty"`
	CreatedAt                    time.Time  `json:"created_at"`
	CreatedByPrincipalID         string     `json:"created_by_principal_id"`
}

// CreateLegalHoldRequest is the wire shape for POST /v1/legal-holds.
type CreateLegalHoldRequest struct {
	ScopeDescription  string   `json:"scope_description"`
	CustodiansObjects []string `json:"custodians_objects,omitempty"`
	Authority         string   `json:"authority"`
	RecordClass       string   `json:"record_class,omitempty"`
	TenantID          string   `json:"tenant_id,omitempty"`
	EntityRef         string   `json:"entity_ref,omitempty"`
	CorrelationID     string   `json:"correlation_id"`
}

// ReleaseLegalHoldRequest is the wire shape for
// POST /v1/legal-holds/{id}/release.
type ReleaseLegalHoldRequest struct {
	ReleaseApprovedByPrincipalID string `json:"release_approved_by_principal_id"`
	CorrelationID                string `json:"correlation_id"`
}

// RetentionResolution is the answer callers actually need before
// deleting/exporting/migrating a record: is it currently blocked by a
// legal hold, and if not, what does the applicable retention policy say.
// Two independent findings, never collapsed into one boolean — a record
// past its minimum retention with NO hold is still a policy decision the
// caller must apply itself; this service never deletes anything.
type RetentionResolution struct {
	Blocked          bool             `json:"blocked"`
	MatchedHold      *LegalHold       `json:"matched_hold,omitempty"`
	ApplicablePolicy *RetentionPolicy `json:"applicable_policy,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrLegalHoldNotFound = errorString("legal hold not found")
	// ErrHoldNotActive is returned when a release request names a hold
	// that is already RELEASED — releasing an already-released hold is
	// rejected, not silently accepted as a new fact.
	ErrHoldNotActive = errorString("legal hold is not currently active")
)
