package store_test

// ZS-SVC-S-001 §9.3 document and record events, written by the transactional
// outbox in the same transaction as the change they announce. Real PostgreSQL.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

func newEventDoc(region *string, title string) (*domain.Document, *domain.DocumentVersion) {
	doc := &domain.Document{
		TenantID: testTenantID, LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title: title, Classification: domain.ClassificationConfidential, RetentionPolicy: "7_YEARS",
		ResidencyRegionCode: region, CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex(title), StorageKey: "key-" + title, SizeBytes: 5,
		ContentType: "application/pdf", DetectedContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}
	return doc, v
}

func TestEvents_CreateDocument_EmitsDocumentCreatedAndVersionCommitted(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	region := "eu-west-1"
	doc, v := newEventDoc(&region, "evt-create")
	clearOutbox(t, pool)
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-evt-1"))

	created := eventsOfType(t, pool, "document_created")
	require.Len(t, created, 1)
	cls, err := s.GetClassification(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	assert.Equal(t, doc.DocumentID, created[0].Payload["document_id"])
	assert.Equal(t, testTenantID, created[0].Payload["tenant_id"])
	assert.Equal(t, cls.ClassificationID, created[0].Payload["classification_id"], "the §9.3 classification_id anchor: the seeded candidate classification")

	committed := eventsOfType(t, pool, "document_version_committed")
	require.Len(t, committed, 1)
	assert.Equal(t, v.DocumentVersionID, committed[0].Payload["version_id"])
	assert.Equal(t, doc.DocumentID, committed[0].Payload["document_id"])
	assert.Equal(t, v.ChecksumSHA256, committed[0].Payload["content_hash"])
	assert.Equal(t, "eu-west-1", committed[0].Payload["region"])

	// The old names are gone: neither document.uploaded nor document.version_created.
	assert.Empty(t, eventsOfType(t, pool, "document.uploaded"))
	assert.Empty(t, eventsOfType(t, pool, "document.version_created"))
	// BIZ-02's own event is unchanged.
	assert.Len(t, eventsOfType(t, pool, "classification.proposed"), 1)
}

func TestEvents_NoRegion_IsNullInTheEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := newEventDoc(nil, "evt-noregion")
	clearOutbox(t, pool)
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr"))
	committed := eventsOfType(t, pool, "document_version_committed")
	require.Len(t, committed, 1)
	assert.Contains(t, committed[0].Payload, "region")
	assert.Nil(t, committed[0].Payload["region"])
}

func TestEvents_AddVersion_EmitsVersionCommitted(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v1 := newEventDoc(nil, "evt-addversion")
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v1, "corr"))
	clearOutbox(t, pool)

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v2"), StorageKey: "key-v2", SizeBytes: 6, ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}
	_, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2, "corr-v2")
	require.NoError(t, err)

	committed := eventsOfType(t, pool, "document_version_committed")
	require.Len(t, committed, 1)
	assert.Equal(t, v2.DocumentVersionID, committed[0].Payload["version_id"])
	assert.Equal(t, sha256Hex("v2"), committed[0].Payload["content_hash"])
	assert.EqualValues(t, 2, committed[0].Payload["version"])
	assert.Empty(t, eventsOfType(t, pool, "document_created"), "adding a version does not create a document")
}

func TestEvents_FailedCreate_LeavesNoEvents(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := newEventDoc(nil, "evt-fail")
	doc.Classification = "BOGUS" // violates the table CHECK: the whole transaction fails
	clearOutbox(t, pool)
	require.Error(t, s.CreateDocument(tenantCtx(), doc, v, "corr"))
	for _, name := range []string{"document_created", "document_version_committed", "classification.proposed"} {
		assert.Empty(t, eventsOfType(t, pool, name), name)
	}
}

func TestEvents_Supersession_IsRecordSuperseded_OtherRelationshipsAreNot(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	prior := createTestRecord(t, s, "evt-prior", string(domain.RecordClassLegalContract), "US-DE")
	next := createTestRecord(t, s, "evt-next", string(domain.RecordClassLegalContract), "US-DE")
	third := createTestRecord(t, s, "evt-third", string(domain.RecordClassLegalContract), "US-DE")
	clearOutbox(t, pool)

	_, err := s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: next.RecordID, TargetRecordID: prior.RecordID, RelationshipType: string(domain.RelationshipSupersedes), CreatedByPrincipalID: "p"})
	require.NoError(t, err)
	sup := eventsOfType(t, pool, "record_superseded")
	require.Len(t, sup, 1)
	assert.Equal(t, next.RecordID, sup[0].Payload["new_record_id"])
	assert.Equal(t, prior.RecordID, sup[0].Payload["prior_record_id"])
	assert.Equal(t, "SUPERSEDES", sup[0].Payload["relation_type"])
	assert.Empty(t, eventsOfType(t, pool, "record.relationship_created"), "a supersession is not announced twice")

	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: third.RecordID, TargetRecordID: next.RecordID, RelationshipType: string(domain.RelationshipCorrects), CreatedByPrincipalID: "p"})
	require.NoError(t, err)
	assert.Len(t, eventsOfType(t, pool, "record_superseded"), 1, "CORRECTS is not a supersession")
	assert.Len(t, eventsOfType(t, pool, "record.relationship_created"), 1, "other relationship types keep their event")
	_ = context.Background
}
