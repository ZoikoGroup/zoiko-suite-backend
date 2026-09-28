package archive_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/archive"
	"zoiko.io/contract/types"
)

func TestArchiveScenarios(t *testing.T) {
	svc := archive.NewArchiveService()
	tenantID := types.MustNewV7()
	now := time.Now().UTC()

	originalPayload := []byte("GENERAL_LEDGER_ARCHIVE_2020_FULL_PACKAGE")
	pkg, err := svc.SealPackage(tenantID, "ARCH_GL_2020", 1000, int64(len(originalPayload)), originalPayload, "SCHED_GL_7YR", "usr_archivist", now)
	if err != nil {
		t.Fatalf("failed to seal archive package: %v", err)
	}

	t.Run("Valid archive integrity passes", func(t *testing.T) {
		err := svc.VerifyIntegrity(pkg, originalPayload)
		if err != nil {
			t.Fatalf("expected integrity verification to pass, got: %v", err)
		}
	})

	t.Run("NP-26: Archive package integrity failure blocks restore/export", func(t *testing.T) {
		tamperedPayload := []byte("GENERAL_LEDGER_ARCHIVE_2020_FULL_PACKAGE_CORRUPTED")
		err := svc.VerifyIntegrity(pkg, tamperedPayload)
		if !errors.Is(err, archive.ErrArchiveIntegrityFailed) {
			t.Fatalf("expected ErrArchiveIntegrityFailed, got: %v", err)
		}
		if pkg.Status != archive.ArchiveStatusTampered {
			t.Fatalf("expected status TAMPERED, got: %s", pkg.Status)
		}
	})

	t.Run("NP-25: Archive restore bypasses IAM/purpose is rejected", func(t *testing.T) {
		// Empty purpose
		_, err := svc.AuthorizeRestore(pkg, "usr_support", "", "PRODUCTION_STAGING", now)
		if !errors.Is(err, archive.ErrRestoreUnauthorizedPurpose) {
			t.Fatalf("expected ErrRestoreUnauthorizedPurpose for empty purpose, got: %v", err)
		}

		// Ad-hoc unauthorized purpose
		_, err = svc.AuthorizeRestore(pkg, "usr_support", "AD_HOC_TEST", "PRODUCTION_STAGING", now)
		if !errors.Is(err, archive.ErrRestoreUnauthorizedPurpose) {
			t.Fatalf("expected ErrRestoreUnauthorizedPurpose for AD_HOC_TEST, got: %v", err)
		}

		// Approved statutory audit purpose
		req, err := svc.AuthorizeRestore(pkg, "usr_tax_lead", "STATUTORY_AUDIT_INSPECTION", "RESTORE_VAULT", now)
		if err != nil {
			t.Fatalf("expected valid restore to be authorized, got: %v", err)
		}
		if !req.ReapplyTombstonesRequired {
			t.Fatalf("expected ReapplyTombstonesRequired to be true")
		}
	})

	t.Run("NP-24: Backup/Archive restored reapplies disposition tombstones before data is operational", func(t *testing.T) {
		record1 := types.MustNewV7()
		record2 := types.MustNewV7()
		record3 := types.MustNewV7() // Disposed / tombstoned record

		restoredSet := []types.UUID{record1, record2, record3}
		tombstones := map[types.UUID]bool{
			record3: true, // Record 3 was destroyed in primary DB prior to restore
		}

		operationalSet := svc.ReapplyTombstones(restoredSet, tombstones)
		if len(operationalSet) != 2 {
			t.Fatalf("expected 2 operational records after re-applying tombstones, got %d", len(operationalSet))
		}
		for _, id := range operationalSet {
			if id == record3 {
				t.Fatalf("tombstoned record 3 must not appear in operational dataset")
			}
		}
	})
}
