package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

func newDocumentForEvidenceTest(t *testing.T, s *store.PgStore, label string) (*domain.Document, *domain.DocumentVersion) {
	t.Helper()
	doc := &domain.Document{
		TenantID: testTenantID, LegalEntityID: uuid.New().String(), Title: "AR Confirmation " + label,
		Classification: domain.ClassificationConfidential, RetentionPolicy: "7_YEARS", CreatedByPrincipalID: "preparer-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex(label), StorageKey: "key-" + label, SizeBytes: 100, ContentType: "application/pdf", CreatedByPrincipalID: "preparer-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v))
	return doc, v
}

// TestPgStore_RegisterEvidence_RequiresSourceAndMethod is the real,
// DB-enforced proof of "evidence source and acquisition method
// mandatory" — the Go-level guard AND migration 000003's own CHECK
// constraints, exercised together.
func TestPgStore_RegisterEvidence_RequiresSourceAndMethod(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := newDocumentForEvidenceTest(t, s, "reqfields")

	_, _, err := s.RegisterEvidence(tenantCtx(), domain.RegisterEvidenceParams{
		DocumentID: doc.DocumentID, LegalEntityID: doc.LegalEntityID, EngagementID: uuid.New().String(),
		EvidenceSource: "", AcquisitionMethod: "EMAIL", RegisteredByPrincipalID: "auditor-1", CorrelationID: "reqfields-1", DocumentVersionID: v.DocumentVersionID,
	})
	require.ErrorIs(t, err, domain.ErrEvidenceSourceRequired)

	_, _, err = s.RegisterEvidence(tenantCtx(), domain.RegisterEvidenceParams{
		DocumentID: doc.DocumentID, LegalEntityID: doc.LegalEntityID, EngagementID: uuid.New().String(),
		EvidenceSource: "CLIENT", AcquisitionMethod: "", RegisteredByPrincipalID: "auditor-1", CorrelationID: "reqfields-2", DocumentVersionID: v.DocumentVersionID,
	})
	require.ErrorIs(t, err, domain.ErrAcquisitionMethodRequired)

	evidence, created, err := s.RegisterEvidence(tenantCtx(), domain.RegisterEvidenceParams{
		DocumentID: doc.DocumentID, LegalEntityID: doc.LegalEntityID, EngagementID: uuid.New().String(),
		EvidenceSource: "CLIENT", AcquisitionMethod: "EMAIL", RegisteredByPrincipalID: "auditor-1", CorrelationID: "reqfields-3", DocumentVersionID: v.DocumentVersionID,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, evidence.EvidenceID)
}

// TestPgStore_EvidenceContradiction_NeverDeletable is the real proof of
// AUD-NEG-021: recording a contradiction is possible, but no code path —
// and no raw SQL UPDATE or DELETE — can remove it once recorded.
func TestPgStore_EvidenceContradiction_NeverDeletable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := newDocumentForEvidenceTest(t, s, "contradiction")

	evidence, _, err := s.RegisterEvidence(tenantCtx(), domain.RegisterEvidenceParams{
		DocumentID: doc.DocumentID, LegalEntityID: doc.LegalEntityID, EngagementID: uuid.New().String(),
		EvidenceSource: "CLIENT", AcquisitionMethod: "EMAIL", RegisteredByPrincipalID: "auditor-1", CorrelationID: "contradiction-1", DocumentVersionID: v.DocumentVersionID,
	})
	require.NoError(t, err)

	contradiction, err := s.RecordContradiction(tenantCtx(), domain.RecordContradictionParams{
		EvidenceID: evidence.EvidenceID, Description: "Confirmed balance disagrees with client ledger", RecordedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	updated, err := s.GetEvidence(tenantCtx(), testTenantID, evidence.EvidenceID)
	require.NoError(t, err)
	require.True(t, updated.StatusFlags.Contradictory, "expected the contradictory flag to be surfaced on the evidence itself")

	_, err = pool.Exec(tenantCtx(), `DELETE FROM evidence_contradictions WHERE contradiction_id=$1`, contradiction.ContradictionID)
	require.Error(t, err, "expected the reject_evidence_contradiction_mutation trigger to refuse deleting a recorded contradiction")

	_, err = pool.Exec(tenantCtx(), `UPDATE evidence_contradictions SET description='tampered' WHERE contradiction_id=$1`, contradiction.ContradictionID)
	require.Error(t, err, "expected the trigger to refuse updating a recorded contradiction too")

	contradictions, err := s.ListContradictions(tenantCtx(), testTenantID, evidence.EvidenceID)
	require.NoError(t, err)
	require.Len(t, contradictions, 1)
}

// TestPgStore_SupersedeEvidence_LinksForwardNeverOverwrites proves
// "no silent replacement": SupersedeEvidence creates a NEW evidence
// version and marks the OLD one superseded via forward-linking, never an
// in-place content change.
func TestPgStore_SupersedeEvidence_LinksForwardNeverOverwrites(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v1 := newDocumentForEvidenceTest(t, s, "supersede")

	evidence, _, err := s.RegisterEvidence(tenantCtx(), domain.RegisterEvidenceParams{
		DocumentID: doc.DocumentID, LegalEntityID: doc.LegalEntityID, EngagementID: uuid.New().String(),
		EvidenceSource: "CLIENT", AcquisitionMethod: "EMAIL", RegisteredByPrincipalID: "auditor-1", CorrelationID: "supersede-1", DocumentVersionID: v1.DocumentVersionID,
	})
	require.NoError(t, err)

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("supersede-v2"), StorageKey: "key-supersede-v2", SizeBytes: 200, ContentType: "application/pdf", CreatedByPrincipalID: "auditor-1"}
	_, err = s.AddVersion(tenantCtx(), doc.DocumentID, v2)
	require.NoError(t, err)

	newVersion, err := s.SupersedeEvidence(tenantCtx(), domain.SupersedeEvidenceParams{EvidenceID: evidence.EvidenceID, NewDocumentVersionID: v2.DocumentVersionID, ActorPrincipalID: "auditor-1"})
	require.NoError(t, err)
	require.Nil(t, newVersion.SupersededByEvidenceVersionID, "the new version itself should not be superseded")

	var oldSupersededBy *string
	var oldDocVersionID string
	err = pool.QueryRow(tenantCtx(), `SELECT superseded_by_evidence_version_id, document_version_id FROM evidence_versions WHERE evidence_id=$1 AND document_version_id=$2`,
		evidence.EvidenceID, v1.DocumentVersionID).Scan(&oldSupersededBy, &oldDocVersionID)
	require.NoError(t, err)
	require.NotNil(t, oldSupersededBy)
	require.Equal(t, newVersion.EvidenceVersionID, *oldSupersededBy)
	require.Equal(t, v1.DocumentVersionID, oldDocVersionID, "the old version's own document_version_id must never change")

	_, err = pool.Exec(tenantCtx(), `UPDATE evidence_versions SET document_version_id=$1 WHERE evidence_id=$2 AND document_version_id=$3`, v2.DocumentVersionID, evidence.EvidenceID, v1.DocumentVersionID)
	require.Error(t, err, "expected the trigger to refuse rewriting an old version's own document_version_id")
}
