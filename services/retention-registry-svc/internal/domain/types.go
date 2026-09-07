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

// ── register filters ─────────────────────────────────────────────────────────
//
// CallerTenantID on both filters is the VERIFIED tenant from X-Tenant-Id, not a
// caller-supplied query parameter, and it is required. That distinction is the
// whole point: a register whose scope came from the query string is a register
// any caller can widen, which is how this platform has previously turned a
// tenant filter into a cross-tenant read.
//
// Both registers deliberately include rows whose own tenant_id IS NULL. NULL
// here does not mean "unscoped, hide it" — it means platform-wide, and a
// platform-wide retention policy or legal hold genuinely applies to every
// tenant. A tenant that could not see the hold freezing its own records would
// read an empty register and conclude deletion was safe. This is NOT the
// "filter that switches itself off" antipattern: that one turns the predicate
// off when the CALLER's identity is absent, which is a control failing open.
// Here the caller's tenant is always required and the NULL being matched belongs
// to the ROW.

type RetentionPolicyFilter struct {
	// CallerTenantID is required. Rows are limited to this tenant plus
	// platform-wide (tenant_id IS NULL) policies.
	CallerTenantID string
	// RecordClass, when set, narrows to one class of record.
	RecordClass string
	// PolicyStatus, when set, narrows to ACTIVE / SUPERSEDED / RETIRED.
	PolicyStatus string
	Limit        int
	Offset       int
}

type LegalHoldFilter struct {
	// CallerTenantID is required, same reasoning as above.
	CallerTenantID string
	// HoldStatus, when set, narrows to ACTIVE or RELEASED.
	HoldStatus string
	// RecordClass, when set, narrows to one class of record.
	RecordClass string
	Limit       int
	Offset      int
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
	// ErrTenantMissing is returned when a register read or a hold lookup
	// arrives with no verified tenant. It is an error and never a default,
	// because defaulting it would turn a dropped X-Tenant-Id header into an
	// unscoped read of every tenant's legal holds.
	ErrTenantMissing = errorString("tenant context missing")
	// ErrInvalidFilter covers an out-of-range limit/offset or a status filter
	// outside the documented vocabulary. Named rather than folded into a 500
	// so a misspelled filter does not read as "no holds exist".
	ErrInvalidFilter = errorString("invalid filter")
	// ErrHoldNotActive is returned when a release request names a hold
	// that is already RELEASED — releasing an already-released hold is
	// rejected, not silently accepted as a new fact.
	ErrHoldNotActive = errorString("legal hold is not currently active")
)
