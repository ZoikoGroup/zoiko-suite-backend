package quality

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrIrreproduciblePopulation is returned when a DQ run lacks reproducible population watermark/identity (GOV-07, DG-012, NP-05).
	ErrIrreproduciblePopulation = errors.New("DQ run population cannot be reproduced; missing or invalid population watermark (blocked per NP-05/DG-012)")

	// ErrPrematureIssueClosure is returned when a material DQ issue is closed without authoritative source fix or reperformance (GOV-08, DG-019, DG-020, NP-06).
	ErrPrematureIssueClosure = errors.New("cannot close material DQ issue without authoritative source remediation and verified reperformance (blocked per NP-06/DG-020)")

	// ErrRuleVersionImmutable is returned when attempting to alter a rule version in-place mid-run (DG-011, NP-04).
	ErrRuleVersionImmutable = errors.New("rule version is immutable; updates must increment version and cannot mutate running evaluations (blocked per NP-04/DG-011)")
)

// Evaluator provides business logic for DQ evaluation runs and issue lifecycle governance.
type Evaluator struct{}

func NewEvaluator() *Evaluator {
	return &Evaluator{}
}

// StartRun initializes an evaluation run, binding it to the exact rule version and reproducible population watermark.
func (e *Evaluator) StartRun(
	tenantID types.UUID,
	rule *DQRule,
	populationIdentity string,
	runBy string,
	startedAt time.Time,
) (*DQRun, error) {
	if rule == nil {
		return nil, errors.New("rule cannot be nil")
	}
	if populationIdentity == "" {
		return nil, ErrIrreproduciblePopulation
	}

	runID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &DQRun{
		RunID:              runID,
		TenantID:           tenantID,
		RuleID:             rule.RuleID,
		RuleVersion:        rule.Version,
		PopulationIdentity: populationIdentity,
		Status:             "PENDING",
		RunBy:              runBy,
		StartedAt:          startedAt,
	}, nil
}

// CompleteRun finalizes metrics, checks thresholds, and flags whether the run passed.
func (e *Evaluator) CompleteRun(
	run *DQRun,
	rule *DQRule,
	evaluatedCount int64,
	passedCount int64,
	completedAt time.Time,
) (*DQResult, error) {
	if run == nil || rule == nil {
		return nil, errors.New("run and rule cannot be nil")
	}
	if evaluatedCount < 0 || passedCount < 0 || passedCount > evaluatedCount {
		return nil, errors.New("invalid evaluated/passed counts")
	}

	run.EvaluatedCount = evaluatedCount
	run.PassedCount = passedCount
	run.FailedCount = evaluatedCount - passedCount

	if evaluatedCount == 0 {
		run.PassRate = 100.0
	} else {
		run.PassRate = (float64(passedCount) / float64(evaluatedCount)) * 100.0
	}

	run.Status = "COMPLETED"
	run.CompletedAt = &completedAt

	passed := run.PassRate >= rule.ThresholdPct

	resultID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &DQResult{
		ResultID:     resultID,
		RunID:        run.RunID,
		TenantID:     run.TenantID,
		RuleID:       rule.RuleID,
		Passed:       passed,
		MetricValues: fmt.Sprintf(`{"pass_rate": %.2f, "evaluated": %d, "passed": %d, "failed": %d}`, run.PassRate, evaluatedCount, passedCount, run.FailedCount),
		EvaluatedAt:  completedAt,
	}, nil
}

// ResolveIssue validates that an issue can only transition to CLOSED when both authoritative source remediation and verified reperformance exist (NP-06, DG-019, DG-020).
func (e *Evaluator) ResolveIssue(
	issue *DQIssue,
	remediationRef string,
	reperformanceRun *DQRun,
	reperformanceResult *DQResult,
	resolvedAt time.Time,
) error {
	if issue == nil {
		return errors.New("issue cannot be nil")
	}
	if remediationRef == "" {
		return fmt.Errorf("%w: missing authoritative remediation reference in source domain", ErrPrematureIssueClosure)
	}
	if reperformanceRun == nil || reperformanceResult == nil {
		return fmt.Errorf("%w: missing reperformance run verification", ErrPrematureIssueClosure)
	}
	if !reperformanceResult.Passed {
		return fmt.Errorf("%w: reperformance run failed to satisfy quality threshold (pass_rate: %.2f)", ErrPrematureIssueClosure, reperformanceRun.PassRate)
	}

	issue.AuthoritativeRemediationRef = remediationRef
	issue.ReperformanceRunID = &reperformanceRun.RunID
	issue.Status = IssueStatusClosed
	issue.ResolvedAt = &resolvedAt

	return nil
}

// UpdateRuleVersion creates a new rule version when definitions or thresholds change, leaving existing versions and ongoing runs unaffected (NP-04, DG-011).
func (e *Evaluator) UpdateRuleVersion(
	current *DQRule,
	newThresholdPct float64,
	updatedBy string,
	createdAt time.Time,
) (*DQRule, error) {
	if current == nil {
		return nil, errors.New("current rule cannot be nil")
	}

	newRule := *current
	newRule.Version = current.Version + 1
	newRule.ThresholdPct = newThresholdPct
	newRule.CreatedBy = updatedBy
	newRule.CreatedAt = createdAt

	return &newRule, nil
}
