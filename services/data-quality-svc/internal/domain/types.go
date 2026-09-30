// Package domain defines the authoritative domain types for
// data-quality-svc (DATA-02, ZS-SVC-N-001 §4).
//
// Design decision (the scoping call this service's whole shape depends
// on): this service has no access to the actual source rows it evaluates
// quality over — it is not the owning business domain's database. So
// "evaluating a rule" here does NOT mean executing rule expressions
// against live data. It means: the CALLER (the owning domain, e.g.
// data-ingestion-svc, or an operator tool) computes its own rule outcomes
// against its own data and submits them as evidence to RunDQ. This
// service's actual job is everything AROUND that evidence — binding it to
// an immutable rule-set version, freezing the population it was measured
// against, running aggregate materiality over the submitted failure
// magnitudes, managing the issue/reperform/certification workflow, and
// sealing a certified outcome. It never repairs source data and it never
// computes rule pass/fail itself.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixRuleSet        = "dqs_"
	PrefixRuleSetVersion = "dqv_"
	PrefixRun            = "dqn_"
	PrefixResult         = "dqr_"
	PrefixIssue          = "dqi_"
	PrefixCertification  = "dqc_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── DQRuleSet / DQRuleSetVersion ────────────────────────────────────────────

// DQRuleSet is the named, stable identity of a rule collection.
// DQRuleSetVersion is the actual versioned content — PublishRuleSetVersion
// always creates a NEW version row; an existing published version is
// never edited in place (doc's own named acceptance test).
type DQRuleSet struct {
	RuleSetID string    `json:"ruleset_id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// RuleDefinition is one rule's identity within a version — this service
// stores WHAT the rule is called and its declared materiality thresholds;
// it never stores or executes the rule's actual matching logic.
type RuleDefinition struct {
	RuleKey                       string  `json:"rule_key"`
	Description                   string  `json:"description"`
	PerRecordMaterialityThreshold *string `json:"per_record_materiality_threshold,omitempty"`
	AggregateMaterialityThreshold *string `json:"aggregate_materiality_threshold,omitempty"`
}

type DQRuleSetVersion struct {
	VersionID     string           `json:"version_id"`
	TenantID      string           `json:"tenant_id"`
	RuleSetID     string           `json:"ruleset_id"`
	VersionNumber int              `json:"version_number"`
	Rules         []RuleDefinition `json:"rules"`
	PublishedAt   time.Time        `json:"published_at"`
	PublishedBy   string           `json:"published_by"`
}

// ── DQRun ────────────────────────────────────────────────────────────────────

type DQRunStatus string

const (
	RunPlanned        DQRunStatus = "Planned"
	RunRunning        DQRunStatus = "Running"
	RunExceptionsOpen DQRunStatus = "ExceptionsOpen"
	RunReperformed    DQRunStatus = "Reperformed"
	RunCertified      DQRunStatus = "Certified"
	RunFailed         DQRunStatus = "Failed"
)

// DQRun is one evaluation attempt against a FROZEN population — a
// caller-supplied, opaque reference plus a caller-supplied row count and
// content hash (the caller computes these from whatever it's evaluating
// and submits them as evidence; this service trusts and records them, it
// never recomputes them — same "frozen populations" doctrine as DATA-07).
type DQRun struct {
	RunID                 string      `json:"run_id"`
	TenantID              string      `json:"tenant_id"`
	RuleSetVersionID      string      `json:"ruleset_version_id"`
	PopulationRef         string      `json:"population_ref"`
	PopulationRowCount    int64       `json:"population_row_count"`
	PopulationContentHash string      `json:"population_content_hash"`
	Status                DQRunStatus `json:"status"`
	SupersedesRunID       *string     `json:"supersedes_run_id,omitempty"`
	StartedAt             time.Time   `json:"started_at"`
	CompletedAt           *time.Time  `json:"completed_at,omitempty"`
	CreatedBy             string      `json:"created_by"`
}

// RuleOutcome is the caller-submitted evidence for one rule's evaluation
// against its own data — pass/fail counts and the SUM of failure
// magnitudes (not just a count), so aggregate materiality can be checked
// against a real total rather than an occurrence count alone.
type RuleOutcome struct {
	RuleKey             string   `json:"rule_key"`
	PassCount           int64    `json:"pass_count"`
	FailCount           int64    `json:"fail_count"`
	FailureMagnitudeSum string   `json:"failure_magnitude_sum"` // decimal string, e.g. "100.00"
	FailingRecordRefs   []string `json:"failing_record_refs,omitempty"`
}

type RunDQRequest struct {
	RuleSetVersionID      string        `json:"ruleset_version_id"`
	PopulationRef         string        `json:"population_ref"`
	PopulationRowCount    int64         `json:"population_row_count"`
	PopulationContentHash string        `json:"population_content_hash"`
	RuleOutcomes          []RuleOutcome `json:"rule_outcomes"`
}

func (r RunDQRequest) Validate() error {
	if r.RuleSetVersionID == "" {
		return fmt.Errorf("ruleset_version_id is required")
	}
	if r.PopulationRef == "" {
		return fmt.Errorf("population_ref is required")
	}
	if r.PopulationRowCount <= 0 {
		return fmt.Errorf("population_row_count must be positive")
	}
	if r.PopulationContentHash == "" {
		return fmt.Errorf("population_content_hash is required")
	}
	if len(r.RuleOutcomes) == 0 {
		return fmt.Errorf("at least one rule_outcome is required")
	}
	for i, ro := range r.RuleOutcomes {
		if ro.RuleKey == "" {
			return fmt.Errorf("rule_outcomes[%d].rule_key is required", i)
		}
		if ro.FailureMagnitudeSum == "" {
			return fmt.Errorf("rule_outcomes[%d].failure_magnitude_sum is required", i)
		}
	}
	return nil
}

// ── DQResult ─────────────────────────────────────────────────────────────────

type DQResultStatus string

const (
	ResultPass DQResultStatus = "Pass"
	ResultFail DQResultStatus = "Fail"
)

// DQResult is one rule's outcome for one run. Materiality is evaluated
// server-side from the submitted magnitudes/counts against the rule's
// declared thresholds — a rule whose aggregate failure magnitude exceeds
// AggregateMaterialityThreshold fails EVEN IF every individual failure
// would pass a per-record threshold (the doc's own named acceptance test:
// "aggregate materiality catches many individually small failures").
type DQResult struct {
	ResultID            string         `json:"result_id"`
	TenantID            string         `json:"tenant_id"`
	RunID               string         `json:"run_id"`
	RuleKey             string         `json:"rule_key"`
	Status              DQResultStatus `json:"status"`
	PassCount           int64          `json:"pass_count"`
	FailCount           int64          `json:"fail_count"`
	FailureMagnitudeSum string         `json:"failure_magnitude_sum"`
	FailingRecordRefs   []string       `json:"failing_record_refs,omitempty"`
	EvaluatedAt         time.Time      `json:"evaluated_at"`
}

// ── DQIssue ──────────────────────────────────────────────────────────────────

type DQIssueStatus string

const (
	IssueOpen     DQIssueStatus = "Open"
	IssueAssigned DQIssueStatus = "Assigned"
)

// DQIssue tracks a failing result requiring disposition. There is
// deliberately NO command that marks an issue resolved directly — the
// only path to clearing one is a Reperform whose new run passes (the
// doc's own named acceptance test: "operator cannot mark a source defect
// fixed without source-domain evidence" — the source-domain evidence IS
// the clean re-run, submitted the same way the original evidence was).
type DQIssue struct {
	IssueID     string        `json:"issue_id"`
	TenantID    string        `json:"tenant_id"`
	RunID       string        `json:"run_id"`
	ResultID    string        `json:"result_id"`
	Description string        `json:"description"`
	Status      DQIssueStatus `json:"status"`
	Assignee    *string       `json:"assignee,omitempty"`
	RaisedAt    time.Time     `json:"raised_at"`
	RaisedBy    string        `json:"raised_by"`
}

type RaiseIssueRequest struct {
	RunID       string `json:"run_id"`
	ResultID    string `json:"result_id"`
	Description string `json:"description"`
}

func (r RaiseIssueRequest) Validate() error {
	if r.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if r.ResultID == "" {
		return fmt.Errorf("result_id is required")
	}
	if r.Description == "" {
		return fmt.Errorf("description is required")
	}
	return nil
}

type AssignIssueRequest struct {
	IssueID  string `json:"issue_id"`
	Assignee string `json:"assignee"`
}

func (r AssignIssueRequest) Validate() error {
	if r.IssueID == "" {
		return fmt.Errorf("issue_id is required")
	}
	if r.Assignee == "" {
		return fmt.Errorf("assignee is required")
	}
	return nil
}

// ── DQCertification ──────────────────────────────────────────────────────────

// DQCertification is the sealed, immutable outcome of a run once it has
// no open issues — same "sealed evidence, sha256, immutable at the
// database" doctrine as data-lineage-svc's ProvenanceManifest.
type DQCertification struct {
	CertificationID string                 `json:"certification_id"`
	TenantID        string                 `json:"tenant_id"`
	RunID           string                 `json:"run_id"`
	Summary         DQCertificationSummary `json:"summary"`
	SummarySHA256   string                 `json:"summary_sha256"`
	CertifiedAt     time.Time              `json:"certified_at"`
	CertifiedBy     string                 `json:"certified_by"`
}

type DQCertificationSummary struct {
	RunID                 string     `json:"run_id"`
	RuleSetVersionID      string     `json:"ruleset_version_id"`
	PopulationRef         string     `json:"population_ref"`
	PopulationRowCount    int64      `json:"population_row_count"`
	PopulationContentHash string     `json:"population_content_hash"`
	Results               []DQResult `json:"results"`
}

var (
	ErrRuleSetNotFound        = errorString("rule set not found")
	ErrRuleSetVersionNotFound = errorString("rule set version not found")
	ErrRunNotFound            = errorString("dq run not found")
	ErrResultNotFound         = errorString("dq result not found")
	ErrIssueNotFound          = errorString("dq issue not found")
	ErrCertificationNotFound  = errorString("dq certification not found")
	ErrRunHasOpenIssues       = errorString("dq run has open issues and cannot be certified")
	ErrRunAlreadyCertified    = errorString("dq run is already certified")
	ErrRunNotInExceptionsOpen = errorString("dq run must be in ExceptionsOpen state to raise an issue")
	ErrIssueAlreadyAssigned   = errorString("dq issue is already assigned")
	ErrIdempotencyKeyReused   = errorString("idempotency key was already used for a different request")
	ErrImmutableViolation     = errorString("this record cannot be mutated in that way")
)
