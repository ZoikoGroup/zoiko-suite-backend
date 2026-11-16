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
