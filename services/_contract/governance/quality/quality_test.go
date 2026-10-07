package quality_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/quality"
	"zoiko.io/contract/types"
)

func TestQualityLifecycle_Scenarios(t *testing.T) {
	evaluator := quality.NewEvaluator()
	tenantID := types.MustNewV7()
	assetID := types.MustNewV7()
	now := time.Now().UTC()

	rule := &quality.DQRule{
		RuleID:        types.MustNewV7(),
		TenantID:      tenantID,
		AssetID:       assetID,
		RuleCode:      "DQ_FIN_GL_NOT_NULL_ACCOUNT",
		Version:       1,
		Dimension:     quality.DQDimensionCompleteness,
		AssertionType: "NOT_NULL",
		ThresholdPct:  100.0,
		Severity:      quality.SeverityCritical,
		IsActive:      true,
		CreatedAt:     now,
		CreatedBy:     "usr_lead_dq",
	}

	t.Run("NP-05: DQ run population cannot be reproduced", func(t *testing.T) {
		_, err := evaluator.StartRun(tenantID, rule, "", "system_worker", now)
		if !errors.Is(err, quality.ErrIrreproduciblePopulation) {
			t.Fatalf("expected ErrIrreproduciblePopulation, got: %v", err)
		}
	})

	t.Run("NP-04: DQ threshold changed mid-run creates new version without mutating running version", func(t *testing.T) {
		watermark := "sha256:abcd1234efgh5678"
		run, err := evaluator.StartRun(tenantID, rule, watermark, "system_worker", now)
		if err != nil {
			t.Fatalf("failed to start run: %v", err)
		}

		// Mid-run change to threshold
		newRule, err := evaluator.UpdateRuleVersion(rule, 99.0, "usr_supervisor", now.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("failed to update rule version: %v", err)
		}

		if newRule.Version != 2 {
			t.Fatalf("expected new rule version 2, got %d", newRule.Version)
		}
		if run.RuleVersion != 1 {
			t.Fatalf("expected active run to remain pinned to version 1, got %d", run.RuleVersion)
		}

		// Complete run under original rule
		res, err := evaluator.CompleteRun(run, rule, 100, 99, now.Add(10*time.Minute))
		if err != nil {
			t.Fatalf("failed to complete run: %v", err)
		}
		if res.Passed {
			t.Fatalf("expected run to fail against 100%% threshold (pass rate 99%%)")
		}
	})

	t.Run("NP-06: Critical DQ issue closed without source fix or reperformance", func(t *testing.T) {
		issue := &quality.DQIssue{
			IssueID:  types.MustNewV7(),
			TenantID: tenantID,
			RuleID:   rule.RuleID,
			AssetID:  assetID,
			Severity: quality.SeverityCritical,
			Status:   quality.IssueStatusOpen,
			OpenedAt: now,
		}

		// Attempt to close with empty remediation
		err := evaluator.ResolveIssue(issue, "", nil, nil, now)
		if !errors.Is(err, quality.ErrPrematureIssueClosure) {
			t.Fatalf("expected ErrPrematureIssueClosure for missing remediation, got: %v", err)
		}

		// Attempt to close with failed reperformance
		failedRun := &quality.DQRun{RunID: types.MustNewV7(), PassRate: 98.0}
		failedResult := &quality.DQResult{Passed: false}
		err = evaluator.ResolveIssue(issue, "PR-10492-fix-gl-nulls", failedRun, failedResult, now)
		if !errors.Is(err, quality.ErrPrematureIssueClosure) {
			t.Fatalf("expected ErrPrematureIssueClosure for failed reperformance, got: %v", err)
		}

		// Legitimate closure with source remediation and passed reperformance
		passedRun := &quality.DQRun{RunID: types.MustNewV7(), PassRate: 100.0}
		passedResult := &quality.DQResult{Passed: true}
		err = evaluator.ResolveIssue(issue, "PR-10492-fix-gl-nulls", passedRun, passedResult, now)
		if err != nil {
			t.Fatalf("expected valid resolution to succeed, got: %v", err)
		}
		if issue.Status != quality.IssueStatusClosed {
			t.Fatalf("expected issue status CLOSED, got: %s", issue.Status)
		}
	})
}
