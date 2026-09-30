package evidence_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/evidence"
	"zoiko.io/contract/types"
)

func TestEvidenceSealing_Scenarios(t *testing.T) {
	sealer := evidence.NewSealer()
	tenantID := types.MustNewV7()
	now := time.Now().UTC()

	content1 := []byte("PAYROLL_RUN_SUMMARY_2026_01")
	hash1 := sha256Hex(content1)

	obj1 := evidence.EvidenceObject{
		ObjectID:          types.MustNewV7(),
		TenantID:          tenantID,
		SubjectEntityType: "PAYROLL_RUN",
		SubjectEntityID:   types.MustNewV7(),
		ObjectType:        evidence.ObjectTypeAuditLogRecord,
		ContentHashSHA256: hash1,
		ByteSize:          int64(len(content1)),
		SealedAt:          now,
		CreatedBy:         "payroll-run-svc",
	}

	manifest, err := sealer.SealManifest(tenantID, "AUDIT_2026_Q1", "STATUTORY_FILING", []evidence.EvidenceObject{obj1}, "usr_auditor", now)
	if err != nil {
		t.Fatalf("failed to seal manifest: %v", err)
	}

	t.Run("Valid manifest verification passes", func(t *testing.T) {
		err := sealer.VerifyManifest(manifest, []evidence.EvidenceObject{obj1})
		if err != nil {
			t.Fatalf("expected verification to pass, got: %v", err)
		}
		if manifest.Status != evidence.ManifestStatusVerified {
			t.Fatalf("expected status VERIFIED, got: %s", manifest.Status)
		}
	})

	t.Run("NP-09: Evidence file changed after sealing triggers tamper detection", func(t *testing.T) {
		tamperedObj := obj1
		tamperedContent := []byte("PAYROLL_RUN_SUMMARY_2026_01_TAMPERED")
		tamperedObj.ContentHashSHA256 = sha256Hex(tamperedContent)

		err := sealer.VerifyManifest(manifest, []evidence.EvidenceObject{tamperedObj})
		if !errors.Is(err, evidence.ErrEvidenceTampered) {
			t.Fatalf("expected ErrEvidenceTampered, got: %v", err)
		}
		if manifest.Status != evidence.ManifestStatusTampered {
			t.Fatalf("expected status TAMPERED, got: %s", manifest.Status)
		}
	})

	t.Run("NP-10: Evidence exported without custody event", func(t *testing.T) {
		// Missing purpose
		_, err := sealer.AuthorizeExport(tenantID, manifest.ManifestID, "usr_compliance", "COMPLIANCE_OFFICER", "", now)
		if !errors.Is(err, evidence.ErrCustodyEventMissing) {
			t.Fatalf("expected ErrCustodyEventMissing for empty purpose, got: %v", err)
		}

		// Valid custody export
		custody, err := sealer.AuthorizeExport(tenantID, manifest.ManifestID, "usr_compliance", "COMPLIANCE_OFFICER", "HMRC_STATUTORY_DISCOVERY", now)
		if err != nil {
			t.Fatalf("expected valid export authorization to succeed, got: %v", err)
		}
		if custody.EventType != evidence.CustodyEventExported {
			t.Fatalf("expected event type EXPORTED, got: %s", custody.EventType)
		}
	})
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
