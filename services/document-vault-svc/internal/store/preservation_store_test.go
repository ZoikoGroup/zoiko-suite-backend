package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/store"
)

// DRC-04 Preservation, Rendition & Content Integrity (ZS-SVC-S-001 §6),
// against real Postgres as the FORCE-RLS app role this service always
// runs under (see requireTestDB / tenantCtx in pg_store_test.go).

func createTestRendition(t *testing.T, s *store.PgStore, v *domain.DocumentVersion, class domain.RenditionClass, label string) *domain.Rendition {
	t.Helper()
	rd, err := s.CreateRendition(tenantCtx(), domain.CreateRenditionParams{
		SourceDocumentVersionID: v.DocumentVersionID, RenditionClass: string(class),
		TransformationProfile: "PDF_TO_PDFA_V1", ChecksumSHA256: sha256Hex(label),
		StorageKey: "rendition-key-" + label, SizeBytes: 500, ContentType: "application/pdf",
		CreatedByPrincipalID: "preservation-bot",
	})
	require.NoError(t, err)
	return rd
}

// DRC-I15: the rendition is a distinct object from its source version,
// with its own hash, and gets its own fixity manifest automatically.
func TestDRC04_CreateRendition_HappyPath_SealsFixityManifest(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-happy")

	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-happy-rendition")
	require.NotEqual(t, v.DocumentVersionID, rd.RenditionID)
	require.Equal(t, v.DocumentVersionID, rd.SourceDocumentVersionID)
	require.Equal(t, domain.RenditionPreservation, rd.RenditionClass)

	manifest, err := s.GetFixityManifestByRendition(tenantCtx(), rd.RenditionID)
	require.NoError(t, err)
	require.Equal(t, domain.FixityVerified, manifest.IntegrityState)
	require.Equal(t, rd.ChecksumSHA256, manifest.SourceHash)
	require.NotNil(t, manifest.RenditionID)
	require.Equal(t, rd.RenditionID, *manifest.RenditionID)
	require.Nil(t, manifest.DocumentVersionID)
}

func TestDRC04_CreateRendition_InvalidClass_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-invalid-class")

	_, err := s.CreateRendition(tenantCtx(), domain.CreateRenditionParams{
		SourceDocumentVersionID: v.DocumentVersionID, RenditionClass: "NOT_A_REAL_CLASS",
		TransformationProfile: "X", ChecksumSHA256: sha256Hex("x"), StorageKey: "k", SizeBytes: 1,
		ContentType: "text/plain", CreatedByPrincipalID: "bot",
	})
	require.ErrorIs(t, err, domain.ErrInvalidRenditionClass)
}

func TestDRC04_CreateRendition_UnknownSourceVersion_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.CreateRendition(tenantCtx(), domain.CreateRenditionParams{
		SourceDocumentVersionID: "00000000-0000-0000-0000-000000000000", RenditionClass: string(domain.RenditionAccess),
		TransformationProfile: "X", ChecksumSHA256: sha256Hex("x"), StorageKey: "k", SizeBytes: 1,
		ContentType: "text/plain", CreatedByPrincipalID: "bot",
	})
	require.ErrorIs(t, err, domain.ErrSourceDocumentVersionNotFound)
}

// A rendition built FROM another rendition (e.g. a redacted copy built
// from an access copy) records real lineage via parent_rendition_id.
func TestDRC04_CreateRendition_WithParentLineage(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-lineage")
	access := createTestRendition(t, s, v, domain.RenditionAccess, "drc04-lineage-access")

	redacted, err := s.CreateRendition(tenantCtx(), domain.CreateRenditionParams{
		SourceDocumentVersionID: v.DocumentVersionID, ParentRenditionID: &access.RenditionID,
		RenditionClass: string(domain.RenditionRedacted), TransformationProfile: "REDACT_V1",
		ChecksumSHA256: sha256Hex("drc04-lineage-redacted"), StorageKey: "k2", SizeBytes: 400,
		ContentType: "application/pdf", CreatedByPrincipalID: "bot",
	})
	require.NoError(t, err)
	require.NotNil(t, redacted.ParentRenditionID)
	require.Equal(t, access.RenditionID, *redacted.ParentRenditionID)
}

// ── Fixity verification and repair ──────────────────────────────────────────

// DRC-I20 + the repair doctrine: a routine re-verification that still
// matches stays VERIFIED.
func TestDRC04_RecordVerification_MatchingHash_StaysVerified(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-verify-ok")
	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-verify-ok-rendition")
	manifest, err := s.GetFixityManifestByRendition(tenantCtx(), rd.RenditionID)
	require.NoError(t, err)

	updated, err := s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: rd.ChecksumSHA256,
		ClaimedOutcome: string(domain.FixityVerified), VerifiedByPrincipalID: "verifier-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.FixityVerified, updated.IntegrityState)
	require.NotNil(t, updated.LastVerifiedAt)
}

// The core structural guarantee: a caller cannot claim VERIFIED when
// the observed hash does not actually match the manifest's source
// hash — the store refuses the claim rather than trusting it. This is
// what makes "never rewrite metadata to fake a hash match" true in
// code, not just in a comment.
func TestDRC04_RecordVerification_ClaimedVerifiedWithWrongHash_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-fake-verify")
	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-fake-verify-rendition")
	manifest, err := s.GetFixityManifestByRendition(tenantCtx(), rd.RenditionID)
	require.NoError(t, err)

	_, err = s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: sha256Hex("totally-different-bytes"),
		ClaimedOutcome: string(domain.FixityVerified), VerifiedByPrincipalID: "bad-actor",
	})
	require.ErrorIs(t, err, domain.ErrFixityHashMismatchClaim)

	// and the manifest itself did not silently flip to VERIFIED
	reloaded, err := s.GetFixityManifest(tenantCtx(), manifest.ManifestID)
	require.NoError(t, err)
	require.Equal(t, domain.FixityVerified, reloaded.IntegrityState) // unchanged from creation
}

// A real mismatch is honestly recorded, then must go through
// StartRepair (not a direct re-verify) before it can become VERIFIED
// again.
func TestDRC04_FixityIncidentAndRepair_FullCycle(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-repair-cycle")
	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-repair-cycle-rendition")
	manifest, err := s.GetFixityManifestByRendition(tenantCtx(), rd.RenditionID)
	require.NoError(t, err)

	mismatched, err := s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: sha256Hex("corrupted-bytes"),
		ClaimedOutcome: string(domain.FixityMismatch), VerifiedByPrincipalID: "verifier-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.FixityMismatch, mismatched.IntegrityState)

	// Cannot re-verify directly out of a failure state.
	_, err = s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: rd.ChecksumSHA256,
		ClaimedOutcome: string(domain.FixityVerified), VerifiedByPrincipalID: "verifier-1",
	})
	require.ErrorIs(t, err, domain.ErrFixityNotVerifiedOrRepairing)

	repairing, err := s.StartRepair(tenantCtx(), domain.StartRepairParams{
		ManifestID: manifest.ManifestID, RepairSourceRef: "replica://backup-2026-10-04",
		StartedByPrincipalID: "repair-bot",
	})
	require.NoError(t, err)
	require.Equal(t, domain.FixityRepairing, repairing.IntegrityState)

	healed, err := s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: rd.ChecksumSHA256,
		ClaimedOutcome: string(domain.FixityVerified), VerifiedByPrincipalID: "repair-bot",
	})
	require.NoError(t, err)
	require.Equal(t, domain.FixityVerified, healed.IntegrityState)
	require.NotNil(t, healed.RepairedByPrincipalID)
	require.Equal(t, "repair-bot", *healed.RepairedByPrincipalID)
}

func TestDRC04_StartRepair_WhenNotFailed_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-repair-not-failed")
	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-repair-not-failed-rendition")
	manifest, err := s.GetFixityManifestByRendition(tenantCtx(), rd.RenditionID)
	require.NoError(t, err)

	_, err = s.StartRepair(tenantCtx(), domain.StartRepairParams{
		ManifestID: manifest.ManifestID, RepairSourceRef: "replica://x", StartedByPrincipalID: "bot",
	})
	require.ErrorIs(t, err, domain.ErrFixityNotFailed)
}

// DRC-I20 applies to originals too: a document_version can get its
// own fixity manifest, independent of any rendition.
func TestDRC04_CreateFixityManifestForVersion_HappyPath(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-original-manifest")

	manifest, err := s.CreateFixityManifestForVersion(tenantCtx(), v.DocumentVersionID, v.ChecksumSHA256, "preservation-bot")
	require.NoError(t, err)
	require.NotNil(t, manifest.DocumentVersionID)
	require.Equal(t, v.DocumentVersionID, *manifest.DocumentVersionID)
	require.Nil(t, manifest.RenditionID)
	require.Equal(t, domain.FixityVerified, manifest.IntegrityState)
}

// ── Redaction profiles ───────────────────────────────────────────────────────

// DRC-I17: a redaction profile can only attach to an actual
// REDACTED_RENDITION — not any other rendition class.
func TestDRC04_CreateRedactionProfile_WrongRenditionClass_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-redaction-wrong-class")
	rd := createTestRendition(t, s, v, domain.RenditionAccess, "drc04-redaction-wrong-class-rendition")

	_, err := s.CreateRedactionProfile(tenantCtx(), domain.CreateRedactionProfileParams{
		RenditionID: rd.RenditionID, Purpose: "external disclosure", RecipientClass: "AUDITOR",
		FieldsRemoved: []string{"ssn"}, ApprovedByPrincipalID: "legal-1",
	})
	require.ErrorIs(t, err, domain.ErrRenditionNotRedactedClass)
}

func TestDRC04_CreateRedactionProfile_HappyPath_AndDuplicateRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-redaction-happy")
	rd := createTestRendition(t, s, v, domain.RenditionRedacted, "drc04-redaction-happy-rendition")

	rp, err := s.CreateRedactionProfile(tenantCtx(), domain.CreateRedactionProfileParams{
		RenditionID: rd.RenditionID, Purpose: "external disclosure", RecipientClass: "AUDITOR",
		FieldsRemoved: []string{"ssn", "bank_account"}, LegalBasisRef: "NDA-2026-04", ApprovedByPrincipalID: "legal-1",
	})
	require.NoError(t, err)
	require.Equal(t, rd.RenditionID, rp.RenditionID)
	require.ElementsMatch(t, []string{"ssn", "bank_account"}, rp.FieldsRemoved)

	_, err = s.CreateRedactionProfile(tenantCtx(), domain.CreateRedactionProfileParams{
		RenditionID: rd.RenditionID, Purpose: "second attempt", RecipientClass: "AUDITOR",
		ApprovedByPrincipalID: "legal-2",
	})
	require.ErrorIs(t, err, domain.ErrRedactionProfileExists)
}

// ── Export packages ──────────────────────────────────────────────────────────

// DRC-I27: the package records exactly what was included (with its
// real hash) and what was deliberately omitted (with a reason, no
// hash) — and the package's own hash is a genuine function of its
// contents, not a caller-supplied value.
func TestDRC04_CreateExportPackage_IncludedAndOmitted(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-export-happy")
	rd := createTestRendition(t, s, v, domain.RenditionAccess, "drc04-export-happy-rendition")

	pkg, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1",
		Items: []domain.ExportPackageItemInput{
			{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID},
			{ItemType: string(domain.ExportItemRendition), RefID: rd.RenditionID, Omit: true, OmissionReason: "recipient not authorized for this rendition"},
		},
	})
	require.NoError(t, err)
	require.Len(t, pkg.PackageHash, 64)
	require.Len(t, pkg.Items, 2)

	var includedCount, omittedCount int
	for _, it := range pkg.Items {
		if it.Included {
			includedCount++
			require.NotNil(t, it.IncludedHash)
			require.Equal(t, v.ChecksumSHA256, *it.IncludedHash)
		} else {
			omittedCount++
			require.Nil(t, it.IncludedHash)
			require.NotNil(t, it.OmissionReason)
		}
	}
	require.Equal(t, 1, includedCount)
	require.Equal(t, 1, omittedCount)

	fetched, err := s.GetExportPackage(tenantCtx(), pkg.PackageID)
	require.NoError(t, err)
	require.Equal(t, pkg.PackageHash, fetched.PackageHash)
	require.Len(t, fetched.Items, 2)
}

func TestDRC04_CreateExportPackage_OmittedWithoutReason_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-export-no-reason")

	_, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1",
		Items: []domain.ExportPackageItemInput{
			{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID, Omit: true},
		},
	})
	require.ErrorIs(t, err, domain.ErrExportOmissionReasonRequired)
}

func TestDRC04_CreateExportPackage_NoItems_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1", Items: nil,
	})
	require.ErrorIs(t, err, domain.ErrNoExportPackageItems)
}

// Two packages built from the exact same inputs produce the exact
// same hash (determinism), and a package with different contents
// produces a different hash.
func TestDRC04_ExportPackageHash_DeterministicAndContentSensitive(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v1 := createTestDocument(t, s, "drc04-hash-a")
	_, v2 := createTestDocument(t, s, "drc04-hash-b")

	pkg1, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1",
		Items:                  []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v1.DocumentVersionID}},
	})
	require.NoError(t, err)

	pkg2, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-2", // different requester, same item
		Items:                  []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v1.DocumentVersionID}},
	})
	require.NoError(t, err)
	require.Equal(t, pkg1.PackageHash, pkg2.PackageHash, "same included content must hash the same regardless of requester")

	pkg3, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1",
		Items:                  []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v2.DocumentVersionID}},
	})
	require.NoError(t, err)
	require.NotEqual(t, pkg1.PackageHash, pkg3.PackageHash, "different content must hash differently")
}

// ── Tenant isolation and immutability ───────────────────────────────────────

func TestDRC04_TenantIsolation(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-tenant-isolation")
	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-tenant-isolation-rendition")

	otherTenantCtx := middleware.WithTenant(context.Background(), "99999999-9999-9999-9999-999999999999")
	_, err := s.GetRendition(otherTenantCtx, rd.RenditionID)
	require.ErrorIs(t, err, domain.ErrRenditionNotFound)
}

// Negative control, mirroring DRC-03's dry-run-boundary proof: raw
// UPDATE against the admin pool is rejected by the trigger, not merely
// filtered out by RLS — proving renditions are immutable at the
// database level, not just by application convention.
func TestDRC04_Rendition_RawUpdateRejectedByTrigger(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "drc04-raw-update")
	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "drc04-raw-update-rendition")

	_, err := pool.Exec(context.Background(), `UPDATE renditions SET storage_key = 'tampered' WHERE rendition_id = $1`, rd.RenditionID)
	require.Error(t, err, "renditions must be immutable at the database level, not just by application convention")
}
