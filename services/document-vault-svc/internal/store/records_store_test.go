package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/store"
)

// DRC-02 Record Declaration & Classification (ZS-SVC-S-001 §4), against
// real Postgres as the FORCE-RLS app role this service always runs
// under (see requireTestDB / tenantCtx in pg_store_test.go).

func createTestDocument(t *testing.T, s *store.PgStore, title string) (*domain.Document, *domain.DocumentVersion) {
	t.Helper()
	doc := &domain.Document{
		TenantID: testTenantID, LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title: title, Classification: domain.ClassificationConfidential,
		RetentionPolicy: "7_YEARS", CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex(title), StorageKey: "key-" + title, SizeBytes: 100,
		ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-doc-"+title))
	return doc, v
}

func baseDeclareParams(doc *domain.Document, v *domain.DocumentVersion, correlationID string) domain.DeclareRecordV2Params {
	return domain.DeclareRecordV2Params{
		DocumentID: doc.DocumentID, DocumentVersionID: v.DocumentVersionID, LegalEntityID: doc.LegalEntityID,
		RecordClass: string(domain.RecordClassAccountingWorkpaper), JurisdictionScope: "US-CA",
		BusinessContext: "Q3 close workpaper", DeclaredByPrincipalID: "reviewer-bob",
		DeclarationReason: "quarter close", CorrelationID: correlationID,
	}
}

// Happy path: an exact document version is declared as a governed
// record with a real record class and jurisdiction scope.
func TestDRC02_DeclareRecordV2_HappyPath(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "happy-path")

	rec, created, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc, v, "corr-declare-1"))
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, domain.RecordStateDeclared, rec.RecordState)
	require.Equal(t, domain.RecordClassAccountingWorkpaper, rec.RecordClass)
	require.Equal(t, "US-CA", rec.JurisdictionScope)

	fetched, err := s.GetRecord(tenantCtx(), rec.RecordID)
	require.NoError(t, err)
	require.Equal(t, rec.RecordID, fetched.RecordID)
}

// Doc-named invariant DRC-I04 (in spirit, via the one-to-one mapping
// §1.3 establishes): each exact document version can be declared as a
// record at most once.
func TestDRC02_DeclareRecordV2_SameVersionTwice_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "declare-twice")

	_, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc, v, "corr-a"))
	require.NoError(t, err)

	_, _, err = s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc, v, "corr-b"))
	require.ErrorIs(t, err, domain.ErrDocumentVersionAlreadyDeclared)
}

// Idempotent replay: the same correlation_id returns the original
// record, never a second one.
func TestDRC02_DeclareRecordV2_IdempotentReplay(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "idempotent")

	params := baseDeclareParams(doc, v, "corr-replay")
	first, created1, err := s.DeclareRecordV2(tenantCtx(), params)
	require.NoError(t, err)
	require.True(t, created1)

	second, created2, err := s.DeclareRecordV2(tenantCtx(), params)
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, first.RecordID, second.RecordID)
}

// Invalid record_class / missing jurisdiction_scope are refused before
// any row is written.
func TestDRC02_DeclareRecordV2_ValidationRefusals(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "validation")

	badClass := baseDeclareParams(doc, v, "corr-badclass")
	badClass.RecordClass = "NOT_A_REAL_CLASS"
	_, _, err := s.DeclareRecordV2(tenantCtx(), badClass)
	require.ErrorIs(t, err, domain.ErrInvalidRecordClass)

	noJurisdiction := baseDeclareParams(doc, v, "corr-nojuris")
	noJurisdiction.JurisdictionScope = ""
	_, _, err = s.DeclareRecordV2(tenantCtx(), noJurisdiction)
	require.ErrorIs(t, err, domain.ErrJurisdictionScopeRequired)
}

// DRC-I04 (verbatim): "A declared record cannot be 'edited'" — proven
// structurally, not just by convention: a raw UPDATE of a declared
// record's facts is rejected at the database.
func TestDRC02_DeclaredRecord_FactsAreImmutable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "immutable-facts")
	rec, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc, v, "corr-immutable"))
	require.NoError(t, err)

	_, err = pool.Exec(context.Background(), `UPDATE records SET jurisdiction_scope = 'EU-DE' WHERE record_id = $1`, rec.RecordID)
	require.Error(t, err, "raw UPDATE of a declared record's facts should have been rejected by the trigger")
}

// §4.5 relationship vocabulary: SUPERSEDES moves the target record to
// SUPERSEDED in the same transaction as the relationship — this is the
// only command in this wave that changes record_state, and it's the
// mechanism DRC-I04's "correction uses supersession" describes.
func TestDRC02_SupersedesRelationship_MovesTargetToSuperseded(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	oldDoc, oldV := createTestDocument(t, s, "superseded-old")
	newDoc, newV := createTestDocument(t, s, "superseded-new")
	oldRec, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(oldDoc, oldV, "corr-old"))
	require.NoError(t, err)
	newRec, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(newDoc, newV, "corr-new"))
	require.NoError(t, err)

	rel, err := s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: newRec.RecordID, TargetRecordID: oldRec.RecordID,
		RelationshipType: string(domain.RelationshipSupersedes), CreatedByPrincipalID: "reviewer-bob",
	})
	require.NoError(t, err)
	require.Equal(t, domain.RelationshipSupersedes, rel.RelationshipType)

	reloaded, err := s.GetRecord(tenantCtx(), oldRec.RecordID)
	require.NoError(t, err)
	require.Equal(t, domain.RecordStateSuperseded, reloaded.RecordState)

	// The new record itself is untouched — only the target moved.
	stillDeclared, err := s.GetRecord(tenantCtx(), newRec.RecordID)
	require.NoError(t, err)
	require.Equal(t, domain.RecordStateDeclared, stillDeclared.RecordState)
}

// A record that has already been superseded cannot be superseded
// again.
func TestDRC02_SupersedesRelationship_AlreadySuperseded_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc1, v1 := createTestDocument(t, s, "double-supersede-1")
	doc2, v2 := createTestDocument(t, s, "double-supersede-2")
	doc3, v3 := createTestDocument(t, s, "double-supersede-3")
	rec1, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc1, v1, "corr-1"))
	require.NoError(t, err)
	rec2, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc2, v2, "corr-2"))
	require.NoError(t, err)
	rec3, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc3, v3, "corr-3"))
	require.NoError(t, err)

	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: rec2.RecordID, TargetRecordID: rec1.RecordID,
		RelationshipType: string(domain.RelationshipSupersedes), CreatedByPrincipalID: "reviewer-bob",
	})
	require.NoError(t, err)

	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: rec3.RecordID, TargetRecordID: rec1.RecordID,
		RelationshipType: string(domain.RelationshipSupersedes), CreatedByPrincipalID: "reviewer-bob",
	})
	require.ErrorIs(t, err, domain.ErrRecordAlreadySuperseded)
}

// A non-SUPERSEDES relationship (e.g. EVIDENCES) is purely evidentiary
// — it never changes either record's state.
func TestDRC02_EvidencesRelationship_DoesNotChangeState(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc1, v1 := createTestDocument(t, s, "evidences-1")
	doc2, v2 := createTestDocument(t, s, "evidences-2")
	rec1, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc1, v1, "corr-ev1"))
	require.NoError(t, err)
	rec2, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc2, v2, "corr-ev2"))
	require.NoError(t, err)

	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: rec1.RecordID, TargetRecordID: rec2.RecordID,
		RelationshipType: string(domain.RelationshipEvidences), CreatedByPrincipalID: "reviewer-bob",
	})
	require.NoError(t, err)

	reloaded1, err := s.GetRecord(tenantCtx(), rec1.RecordID)
	require.NoError(t, err)
	require.Equal(t, domain.RecordStateDeclared, reloaded1.RecordState)
	reloaded2, err := s.GetRecord(tenantCtx(), rec2.RecordID)
	require.NoError(t, err)
	require.Equal(t, domain.RecordStateDeclared, reloaded2.RecordState)

	rels, err := s.ListRecordRelationships(tenantCtx(), rec1.RecordID)
	require.NoError(t, err)
	require.Len(t, rels, 1)
	require.Equal(t, domain.RelationshipEvidences, rels[0].RelationshipType)
}

// A record cannot have a relationship to itself, and the same exact
// relationship cannot be recorded twice.
func TestDRC02_RecordRelationship_SelfReferenceAndDuplicateRefused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "self-ref")
	rec, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc, v, "corr-self"))
	require.NoError(t, err)

	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: rec.RecordID, TargetRecordID: rec.RecordID,
		RelationshipType: string(domain.RelationshipEvidences), CreatedByPrincipalID: "reviewer-bob",
	})
	require.ErrorIs(t, err, domain.ErrRecordRelationshipSelfReference)

	doc2, v2 := createTestDocument(t, s, "self-ref-2")
	rec2, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc2, v2, "corr-self2"))
	require.NoError(t, err)

	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: rec.RecordID, TargetRecordID: rec2.RecordID,
		RelationshipType: string(domain.RelationshipEvidences), CreatedByPrincipalID: "reviewer-bob",
	})
	require.NoError(t, err)
	_, err = s.CreateRecordRelationship(tenantCtx(), domain.CreateRecordRelationshipParams{
		SourceRecordID: rec.RecordID, TargetRecordID: rec2.RecordID,
		RelationshipType: string(domain.RelationshipEvidences), CreatedByPrincipalID: "reviewer-bob",
	})
	require.ErrorIs(t, err, domain.ErrDuplicateRecordRelationship)
}

// Declaring a record against a non-existent document version is
// refused outright — fail closed, never silently created against
// nothing.
func TestDRC02_DeclareRecordV2_UnknownDocumentVersion_Refused(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, _ := createTestDocument(t, s, "unknown-version")

	params := domain.DeclareRecordV2Params{
		DocumentID: doc.DocumentID, DocumentVersionID: "00000000-0000-0000-0000-000000000000",
		LegalEntityID: doc.LegalEntityID, RecordClass: string(domain.RecordClassLegalContract),
		JurisdictionScope: "US-NY", DeclaredByPrincipalID: "reviewer-bob", CorrelationID: "corr-unknown",
	}
	_, _, err := s.DeclareRecordV2(tenantCtx(), params)
	require.ErrorIs(t, err, domain.ErrDocumentVersionNotFoundForRecord)
}

// Tenant isolation: cross-tenant reads are denied, not an
// application-level filter — proven via the FORCE-RLS app role every
// test in this suite already runs under.
func TestDRC02_TenantIsolation(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "tenant-isolation")
	rec, _, err := s.DeclareRecordV2(tenantCtx(), baseDeclareParams(doc, v, "corr-tenant"))
	require.NoError(t, err)

	otherTenantCtx := middleware.WithTenant(context.Background(), "99999999-9999-9999-9999-999999999999")
	_, err = s.GetRecord(otherTenantCtx, rec.RecordID)
	require.True(t, errors.Is(err, domain.ErrRecordNotFound))
}
