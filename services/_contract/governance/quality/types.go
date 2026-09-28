package quality

import (
	"time"

	"zoiko.io/contract/types"
)

// DQDimension represents the 6 canonical data quality dimensions (ZS-DATA-GOV-001 §6.1, DG-013..DG-017).
type DQDimension string

const (
	DQDimensionCompleteness DQDimension = "COMPLETENESS" // Mandatory fields populated (DG-013)
	DQDimensionValidity     DQDimension = "VALIDITY"     // Format, range, dictionary, reference data validity (DG-014)
	DQDimensionAccuracy     DQDimension = "ACCURACY"     // Mathematical or physical reality consistency
	DQDimensionConsistency  DQDimension = "CONSISTENCY"  // Cross-system / cross-book balance integrity (DG-017)
	DQDimensionTimeliness   DQDimension = "TIMELINESS"   // Freshness within approved latency/watermark (DG-016)
	DQDimensionUniqueness   DQDimension = "UNIQUENESS"   // Duplicate detection on unique keys (DG-015)
)

// IssueSeverity indicates the criticality of a data quality finding (§7, DG-018).
type IssueSeverity string

const (
	SeverityLow      IssueSeverity = "LOW"
	SeverityMedium   IssueSeverity = "MEDIUM"
	SeverityHigh     IssueSeverity = "HIGH"
	SeverityCritical IssueSeverity = "CRITICAL"
)

// IssueStatus tracks the lifecycle of a governed quality issue (§7, DG-018..DG-020).
type IssueStatus string

const (
	IssueStatusOpen       IssueStatus = "OPEN"
	IssueStatusAssigned   IssueStatus = "ASSIGNED"
	IssueStatusRemediated IssueStatus = "REMEDIATED" // Source fixed, awaiting reperformance
	IssueStatusClosed     IssueStatus = "CLOSED"     // Verified via reperformance run
)

// DQRule defines an immutable, versioned quality assertion (§6.2, DG-011, NP-04).
type DQRule struct {
	RuleID         types.UUID  `json:"rule_id"`
	TenantID       types.UUID  `json:"tenant_id"`
	AssetID        types.UUID  `json:"asset_id"`
	RuleCode       string      `json:"rule_code"`
	Version        int         `json:"version"` // Incremented on change; rules are immutable by version
	Dimension      DQDimension `json:"dimension"`
	AssertionType  string      `json:"assertion_type"` // e.g. "NOT_NULL", "REFERENCE_EXISTS", "RATE_RANGE"
	Parameters     string      `json:"parameters"`     // JSON serialized configuration
	ThresholdPct   float64     `json:"threshold_pct"`  // Required pass threshold e.g. 100.0 or 99.5
	Severity       IssueSeverity `json:"severity"`
	IsActive       bool        `json:"is_active"`
	CreatedAt      time.Time   `json:"created_at"`
	CreatedBy      string      `json:"created_by"`
}

// DQRun models an evaluation execution bound to a reproducible population watermark (§6.2, DG-012, NP-05).
type DQRun struct {
	RunID              types.UUID `json:"run_id"`
	TenantID           types.UUID `json:"tenant_id"`
	RuleID             types.UUID `json:"rule_id"`
	RuleVersion        int        `json:"rule_version"` // Exact rule version evaluated (pinned)
	PopulationIdentity string     `json:"population_identity"` // Watermark or population query SHA-256
	EvaluatedCount     int64      `json:"evaluated_count"`
	PassedCount        int64      `json:"passed_count"`
	FailedCount        int64      `json:"failed_count"`
	PassRate           float64    `json:"pass_rate"`
	Status             string     `json:"status"` // "PENDING", "COMPLETED", "FAILED"
	RunBy              string     `json:"run_by"`
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

// DQResult records granular pass/fail metrics from a run.
type DQResult struct {
	ResultID         types.UUID `json:"result_id"`
	RunID            types.UUID `json:"run_id"`
	TenantID         types.UUID `json:"tenant_id"`
	RuleID           types.UUID `json:"rule_id"`
	Passed           bool       `json:"passed"`
	MetricValues     string     `json:"metric_values"`     // JSON metrics
	SampleViolations string     `json:"sample_violations"` // JSON sample of failed records
	EvaluatedAt      time.Time  `json:"evaluated_at"`
}

// DQIssue models a governed quality exception requiring authoritative remediation (§7, DG-018..DG-020, NP-06).
type DQIssue struct {
	IssueID                     types.UUID    `json:"issue_id"`
	TenantID                    types.UUID    `json:"tenant_id"`
	RunID                       types.UUID    `json:"run_id"`
	RuleID                      types.UUID    `json:"rule_id"`
	AssetID                     types.UUID    `json:"asset_id"`
	Severity                    IssueSeverity `json:"severity"`
	Status                      IssueStatus   `json:"status"`
	AssignedTo                  string        `json:"assigned_to,omitempty"`
	SLADueAt                    time.Time     `json:"sla_due_at"`
	AuthoritativeRemediationRef string        `json:"authoritative_remediation_ref,omitempty"` // Reference to source domain fix
	ReperformanceRunID          *types.UUID   `json:"reperformance_run_id,omitempty"`
	OpenedAt                    time.Time     `json:"opened_at"`
	ResolvedAt                  *time.Time    `json:"resolved_at,omitempty"`
}
