package certification_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/certification"
	"zoiko.io/contract/types"
)

func TestCertificationScenarios(t *testing.T) {
	svc := certification.NewService()
	tenantID := types.MustNewV7()
	assetID := types.MustNewV7()
	now := time.Now().UTC()

	manifestID := types.MustNewV7()
	dqRunID := types.MustNewV7()

	cert, err := svc.IssueCertification(
		tenantID,
		assetID,
		certification.ClassC3FinancialRegulatory,
		&manifestID,
		&dqRunID,
		"sha256:lineage_proof_hash_1234",
		"usr_controller",
		now,
		365*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("failed to issue C3 certification: %v", err)
	}

	t.Run("NP-30: Owner changes after certification invalidates certification", func(t *testing.T) {
		svc.InvalidateOnOwnerChange(cert, "usr_new_controller", "usr_old_controller")
		if cert.Status != certification.CertStatusInvalidated {
			t.Fatalf("expected certification status INVALIDATED, got: %s", cert.Status)
		}
	})

	t.Run("NP-27: AI embedding has no source lineage is rejected", func(t *testing.T) {
		err := svc.AssertAILineage(false)
		if !errors.Is(err, certification.ErrMissingAILineage) {
			t.Fatalf("expected ErrMissingAILineage, got: %v", err)
		}
		err = svc.AssertAILineage(true)
		if err != nil {
			t.Fatalf("expected valid lineage to succeed, got: %v", err)
		}
	})

	t.Run("NP-28: Derived aggregate claims declassification without rule", func(t *testing.T) {
		// Sources are RESTRICTED and INTERNAL
		sources := []string{"RESTRICTED", "INTERNAL"}

		// Claiming "INTERNAL" without approved declassification rule -> rejected
		_, err := svc.ResolveDerivedClassification(sources, false, "INTERNAL")
		if !errors.Is(err, certification.ErrUnauthorizedDeclassification) {
			t.Fatalf("expected ErrUnauthorizedDeclassification, got: %v", err)
		}

		// With approved rule -> permitted
		resolved, err := svc.ResolveDerivedClassification(sources, true, "INTERNAL")
		if err != nil || resolved != "INTERNAL" {
			t.Fatalf("expected declassification with rule to succeed, got: %s, err: %v", resolved, err)
		}
	})

	t.Run("NP-29: Cross-region copy not registered is rejected", func(t *testing.T) {
		unregisteredTransfer := &certification.DataTransferRecord{
			TransferID:   types.MustNewV7(),
			TenantID:     tenantID,
			SourceRegion: "eu-west-1",
			TargetRegion: "us-east-1",
			IsRegistered: false,
		}
		err := svc.ValidateCrossRegionTransfer(unregisteredTransfer)
		if !errors.Is(err, certification.ErrUnregisteredTransfer) {
			t.Fatalf("expected ErrUnregisteredTransfer, got: %v", err)
		}
	})

	t.Run("NP-35: Governance DB unavailable fails closed", func(t *testing.T) {
		err := svc.FailClosedOnDBUnavailable(false)
		if !errors.Is(err, certification.ErrFailClosedDBUnavailable) {
			t.Fatalf("expected ErrFailClosedDBUnavailable when db down, got: %v", err)
		}
		err = svc.FailClosedOnDBUnavailable(true)
		if err != nil {
			t.Fatalf("expected db connected to pass, got: %v", err)
		}
	})

	t.Run("NP-36: Generic status PATCH rejected; only named commands allowed", func(t *testing.T) {
		err := svc.AssertNamedCommandOnly("PATCH", false)
		if !errors.Is(err, certification.ErrGenericPatchProhibited) {
			t.Fatalf("expected ErrGenericPatchProhibited for PATCH, got: %v", err)
		}
		err = svc.AssertNamedCommandOnly("POST", true)
		if err != nil {
			t.Fatalf("expected named command POST to pass, got: %v", err)
		}
	})
}
