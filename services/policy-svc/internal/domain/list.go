package domain

import "time"

// ListQuery is one page of a console register (API-4): the filters the
// handler has validated, how many rows to read (the page size plus one, so
// the handler can tell whether there is a next page) and where to start.
// Every register is ordered newest first, with the id as the tie-break, so
// an offset cursor names the same position on the next call.
type ListQuery struct {
	Filters map[string]string
	Limit   int
	Offset  int
}

// Filter returns the named filter's value, "" when absent.
func (q ListQuery) Filter(name string) string {
	if q.Filters == nil {
		return ""
	}
	return q.Filters[name]
}

// PolicySummary is the console's KPI row for the caller's tenant: the
// versions it can see (its own and platform-global ones) by lifecycle state,
// its own evaluations and evidence, and its own control tests and
// attestations. Every number is a count of rows, never an estimate.
type PolicySummary struct {
	TenantID string    `json:"tenant_id"`
	AsOf     time.Time `json:"as_of"`

	PolicyFamilies         int64 `json:"policy_families"`
	VersionsDraft          int64 `json:"versions_draft"`
	VersionsInReview       int64 `json:"versions_in_review"`
	VersionsApproved       int64 `json:"versions_approved"`
	VersionsScheduled      int64 `json:"versions_scheduled"`
	VersionsActive         int64 `json:"versions_active"`
	VersionsSuspended      int64 `json:"versions_suspended"`
	SuspensionsAwaitReview int64 `json:"suspensions_awaiting_review"`
	Evaluations24h         int64 `json:"evaluations_24h"`
	EvaluationsApproval24h int64 `json:"evaluations_approval_required_24h"`
	EvidencePending        int64 `json:"evidence_pending"`
	EvidenceFailed         int64 `json:"evidence_failed"`
	ControlTestDefinitions int64 `json:"control_test_definitions"`
	ControlTestsFailing    int64 `json:"control_tests_failing"`
	AttestationsActive     int64 `json:"attestations_active"`
	AttestationsRevoked    int64 `json:"attestations_revoked"`
}

// FamilyStatus is the registry status card of one policy family for the
// caller's tenant (ZS-GOV-POL-001 §9): how many families and versions exist,
// which versions govern now or are scheduled to, with their effective dates,
// approval and activation trail, and the decisions of the last 24 hours by
// outcome.
type FamilyStatus struct {
	PolicyType        string             `json:"policy_type"`
	Families          int64              `json:"families"`
	VersionsByStatus  map[string]int64   `json:"versions_by_status"`
	GoverningVersions []GoverningVersion `json:"governing_versions"`
	Decisions24h      map[string]int64   `json:"decisions_24h"`
}

// GoverningVersion is an ACTIVE or SCHEDULED version and who put it there.
type GoverningVersion struct {
	PolicyID               string     `json:"policy_id"`
	PolicyCode             string     `json:"policy_code"`
	PolicyVersionID        string     `json:"policy_version_id"`
	VersionNumber          int        `json:"version_number"`
	VersionStatus          string     `json:"version_status"`
	ScopeType              string     `json:"scope_type"`
	TenantID               *string    `json:"tenant_id"`
	LegalEntityID          *string    `json:"legal_entity_id"`
	EffectiveFrom          time.Time  `json:"effective_from"`
	EffectiveTo            *time.Time `json:"effective_to"`
	ArtifactDigest         string     `json:"artifact_digest"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	SubmittedByPrincipalID *string    `json:"submitted_by_principal_id,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovalReference      *string    `json:"approval_reference,omitempty"`
	ActivatedByPrincipalID *string    `json:"activated_by_principal_id,omitempty"`
	ActivatedAt            *time.Time `json:"activated_at,omitempty"`
}
