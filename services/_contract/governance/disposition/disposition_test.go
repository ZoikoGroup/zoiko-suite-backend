package disposition_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/disposition"
	"zoiko.io/contract/types"
)

func TestDispositionScenarios(t *testing.T) {
	orch := disposition.NewOrchestrator()
	tenantID := types.MustNewV7()
	now := time.Now().UTC()

	id1 := types.MustNewV7()
	id2 := types.MustNewV7()
	id3 := types.MustNewV7()
	frozenPopulation := []types.UUID{id1, id2, id3}

	batch, err := orch.CreateBatch(tenantID, "BATCH_2026_Q3_TAX_INVOICES", frozenPopulation, disposition.MethodSecurePurge, "v1.0.0", "RETENTION_EXPIRED", now)
	if err != nil {
		t.Fatalf("failed to create batch: %v", err)
	}

	err = orch.ApproveBatch(batch, "usr_compliance_lead", now)
	if err != nil {
		t.Fatalf("failed to approve batch: %v", err)
	}

	t.Run("NP-34: Destruction certificate created before verification is rejected", func(t *testing.T) {
		conv := disposition.ProjectionConvergenceResult{CachesPurged: true, SearchIndexPurged: true, AnalyticsMartPurged: true, ResidualCount: 0}
		// Attempting to issue before execution
		_, err := orch.IssueDestructionCertificate(batch, conv, "usr_certifier", now)
		if !errors.Is(err, disposition.ErrUnverifiedDestructionCertificate) {
			t.Fatalf("expected ErrUnverifiedDestructionCertificate, got: %v", err)
		}
	})

	t.Run("DG-042: Segregation of duties - executor cannot be sole approver", func(t *testing.T) {
		err := orch.ExecuteBatch(batch, "usr_compliance_lead", frozenPopulation, false, now)
		if !errors.Is(err, disposition.ErrSoDViolation) {
			t.Fatalf("expected ErrSoDViolation, got: %v", err)
		}
	})

	t.Run("NP-20: Disposition batch population changes after approval", func(t *testing.T) {
		// Altered population (missing id3)
		alteredPopulation := []types.UUID{id1, id2}
		err := orch.ExecuteBatch(batch, "usr_executor_ops", alteredPopulation, false, now)
		if !errors.Is(err, disposition.ErrBatchPopulationAltered) {
			t.Fatalf("expected ErrBatchPopulationAltered, got: %v", err)
		}
	})

	t.Run("NP-21: Disposition executor adds records not in approved batch", func(t *testing.T) {
		unapprovedID := types.MustNewV7()
		expandedPopulation := []types.UUID{id1, id2, id3, unapprovedID}
		err := orch.ExecuteBatch(batch, "usr_executor_ops", expandedPopulation, false, now)
		if !errors.Is(err, disposition.ErrBatchPopulationAltered) {
			t.Fatalf("expected ErrBatchPopulationAltered or ErrUnapprovedRecordInBatch, got: %v", err)
		}
	})

	t.Run("NP-33: Disposition execution partially fails", func(t *testing.T) {
		failBatch, _ := orch.CreateBatch(tenantID, "BATCH_PARTIAL_TEST", frozenPopulation, disposition.MethodSecurePurge, "v1", "EVIDENCE", now)
		_ = orch.ApproveBatch(failBatch, "usr_lead_a", now)

		err := orch.ExecuteBatch(failBatch, "usr_ops_b", frozenPopulation, true, now)
		if err == nil {
			t.Fatalf("expected execution failure")
		}
		if failBatch.Status != disposition.BatchStatusPartiallyFailed {
			t.Fatalf("expected status PARTIALLY_FAILED, got: %s", failBatch.Status)
		}
	})

	t.Run("NP-22 & NP-23: Projection convergence failure blocks destruction certificate", func(t *testing.T) {
		// Execute successfully
		err := orch.ExecuteBatch(batch, "usr_executor_ops", frozenPopulation, false, now)
		if err != nil {
			t.Fatalf("expected execution to succeed, got: %v", err)
		}

		// Projection still retains residual records (e.g. search vector not purged)
		convResidual := disposition.ProjectionConvergenceResult{
			CachesPurged:        false,
			SearchIndexPurged:   false,
			AnalyticsMartPurged: true,
			ResidualCount:       2,
		}
		_, err = orch.IssueDestructionCertificate(batch, convResidual, "usr_certifier", now)
		if !errors.Is(err, disposition.ErrProjectionConvergenceFailed) {
			t.Fatalf("expected ErrProjectionConvergenceFailed, got: %v", err)
		}

		// Fully converged -> certificate issued
		convClean := disposition.ProjectionConvergenceResult{
			CachesPurged:        true,
			SearchIndexPurged:   true,
			AnalyticsMartPurged: true,
			ResidualCount:       0,
		}
		cert, err := orch.IssueDestructionCertificate(batch, convClean, "usr_certifier", now)
		if err != nil {
			t.Fatalf("expected certificate to be issued, got: %v", err)
		}
		if cert.VerifiedPurgedCount != 3 {
			t.Fatalf("expected 3 verified purged records, got %d", cert.VerifiedPurgedCount)
		}
	})
}
