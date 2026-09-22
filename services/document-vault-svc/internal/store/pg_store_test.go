package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/store"
)

// requireTestDB skips the test unless TEST_DATABASE_URL is set (CI/local dev
// with a real Postgres instance) — same gating pattern used across the repo.
func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Fresh schema per test run.
	_, err = pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS custody_entries;
		DROP TABLE IF EXISTS evidence_contradictions;
		DROP TABLE IF EXISTS evidence_procedure_links;
		DROP TABLE IF EXISTS evidence_reliability_assessments;
		DROP TABLE IF EXISTS evidence_versions;
		DROP TABLE IF EXISTS audit_evidence;
		DROP TABLE IF EXISTS document_links;
		DROP TABLE IF EXISTS document_access_log;
		DROP TABLE IF EXISTS document_versions;
		DROP TABLE IF EXISTS documents;
	`)
	require.NoError(t, err)

	// Replay EVERY migration in order, DISCOVERED rather than listed. Applying
	// only the initial schema builds a database no deployment has ever had and
	// silently skips whatever the later migrations assert — here 000002's FORCE
	// row-level security and the invariants beside it. Globbing means migration
	// 000003 is picked up without anyone remembering to add it. Sorting by
	// filename is what orders them, which is what the numeric prefix is for.
	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")

	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no migrations found in %s", migDir)
	sort.Strings(migrations)

	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		require.NoError(t, err, "reading migration %s", filepath.Base(path))
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoError(t, err, "applying migration %s", filepath.Base(path))
	}

	return pool
}

// testTenantID is the scope this suite runs under. Documents are tenant-scoped,
// and the store reads the tenant from the context — which in production only
// the tenant middleware populates, so a bare context.Background() is an
// unscoped call that the store now refuses outright.
const testTenantID = "11111111-1111-1111-1111-111111111111"

// tenantCtx builds the context the tenant middleware would have built.
func tenantCtx() context.Context {
	return middleware.WithTenant(context.Background(), testTenantID)
}

// sha256Hex returns a genuine 64-character SHA-256 digest for a label.
//
// Migration 000002 requires length(checksum_sha256) = 64, because a checksum is
// the integrity control that every read recomputes and a short placeholder is
// not one. These fixtures previously used strings like "v1sum", which the
// constraint correctly refuses. Deriving each from its label keeps the digests
// distinct, reproducible, and the right shape.
func sha256Hex(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func TestPgStore_CreateDocument_And_FindByID(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Board Resolution", Classification: domain.ClassificationConfidential,
		RetentionPolicy: "7_YEARS", CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("abc123"), StorageKey: "key-1", SizeBytes: 100,
		ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}

	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-108"))
	require.NotEmpty(t, doc.DocumentID)
	require.Equal(t, 1, doc.CurrentVersion)
	require.NotEmpty(t, v.DocumentVersionID)

	found, err := s.FindDocumentByID(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	require.Equal(t, doc.Title, found.Title)
	require.Equal(t, domain.StatusActive, found.Status)
}

func TestPgStore_AddVersion_BumpsCurrentVersion_PreservesLineage(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Policy Doc", Classification: domain.ClassificationInternal,
		CreatedByPrincipalID: "principal-1",
	}
	v1 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v1sum"), StorageKey: "key-1", SizeBytes: 10,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v1, "corr-131"))

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v2sum"), StorageKey: "key-2", SizeBytes: 20,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-2"}
	updated, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2, "corr-addversion")
	require.NoError(t, err)
	require.Equal(t, 2, updated.CurrentVersion)

	versions, err := s.ListVersions(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, versions, 2, "both versions must remain — lineage is append-only, never overwritten")
	require.Equal(t, sha256Hex("v1sum"), versions[0].ChecksumSHA256)
	require.Equal(t, sha256Hex("v2sum"), versions[1].ChecksumSHA256)
}

func newDocForDeclareTest(t *testing.T, s *store.PgStore, label string) *domain.Document {
	t.Helper()
	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Declare Test " + label, Classification: domain.ClassificationInternal,
		CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("declare-" + label), StorageKey: "key-declare-" + label,
		SizeBytes: 5, ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-declare-"+label))
	return doc
}

// TestPgStore_DeclareRecord_Succeeds_ThenRejectsRedeclaration is the real
// proof of BIZ-01's central concept: DeclareRecord succeeds once, records
// the CURRENT version, and — the negative control — a second
// DeclareRecord call on the same document is rejected, not silently
// re-applied.
func TestPgStore_DeclareRecord_Succeeds_ThenRejectsRedeclaration(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "redeclare")

	declared, err := s.DeclareRecord(tenantCtx(), domain.DeclareRecordParams{
		DocumentID: doc.DocumentID, DeclaredByPrincipalID: "declarer-1",
	}, "corr-declare-1")
	require.NoError(t, err)
	require.NotNil(t, declared.DeclaredAt)
	require.NotNil(t, declared.DeclaredVersion)
	require.Equal(t, 1, *declared.DeclaredVersion)
	require.Equal(t, "declarer-1", *declared.DeclaredByPrincipalID)

	_, err = s.DeclareRecord(tenantCtx(), domain.DeclareRecordParams{
		DocumentID: doc.DocumentID, DeclaredByPrincipalID: "declarer-2",
	}, "corr-declare-2")
	require.ErrorIs(t, err, domain.ErrDocumentAlreadyDeclared)

	// Negative control at the DB layer: a raw UPDATE attempting to change
	// the declaration must be refused by migration 000005's own trigger,
	// not just the application-layer CAS.
	_, err = pool.Exec(tenantCtx(), `UPDATE documents SET declared_by_principal_id = 'tampered' WHERE document_id = $1`, doc.DocumentID)
	require.Error(t, err, "expected the trigger to refuse mutating an existing declaration")
}

// TestPgStore_SupersedeDocument_LinksForwardThenRejectsSecondSupersede
// mirrors the evidence-supersede proof this same service already has for
// AUD-06: forward-link once, never twice, never an in-place rewrite.
func TestPgStore_SupersedeDocument_LinksForwardThenRejectsSecondSupersede(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	original := newDocForDeclareTest(t, s, "supersede-orig")
	replacement := newDocForDeclareTest(t, s, "supersede-new")

	updated, err := s.SupersedeDocument(tenantCtx(), domain.SupersedeDocumentParams{
		DocumentID: original.DocumentID, SupersededByDocumentID: replacement.DocumentID, ActorPrincipalID: "actor-1",
	}, "corr-supersede-1")
	require.NoError(t, err)
	require.Equal(t, domain.StatusSuperseded, updated.Status)
	require.NotNil(t, updated.SupersededByDocumentID)
	require.Equal(t, replacement.DocumentID, *updated.SupersededByDocumentID)

	// A second, different replacement must be rejected — supersession is
	// exactly-once.
	third := newDocForDeclareTest(t, s, "supersede-third")
	_, err = s.SupersedeDocument(tenantCtx(), domain.SupersedeDocumentParams{
		DocumentID: original.DocumentID, SupersededByDocumentID: third.DocumentID, ActorPrincipalID: "actor-1",
	}, "corr-supersede-2")
	require.ErrorIs(t, err, domain.ErrDocumentAlreadySuperseded)

	// Negative control at the DB layer.
	_, err = pool.Exec(tenantCtx(), `UPDATE documents SET superseded_by_document_id = $1 WHERE document_id = $2`, third.DocumentID, original.DocumentID)
	require.Error(t, err, "expected the trigger to refuse re-pointing an existing supersede link")
}

// TestPgStore_SupersedeDocument_RejectsSelfAndUnknownTarget covers the two
// input-validation paths: self-supersede and a nonexistent replacement.
func TestPgStore_SupersedeDocument_RejectsSelfAndUnknownTarget(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "self")

	_, err := s.SupersedeDocument(tenantCtx(), domain.SupersedeDocumentParams{
		DocumentID: doc.DocumentID, SupersededByDocumentID: doc.DocumentID, ActorPrincipalID: "actor-1",
	}, "corr-self")
	require.ErrorIs(t, err, domain.ErrCannotSupersedeSelf)

	_, err = s.SupersedeDocument(tenantCtx(), domain.SupersedeDocumentParams{
		DocumentID: doc.DocumentID, SupersededByDocumentID: "00000000-0000-0000-0000-000000000000", ActorPrincipalID: "actor-1",
	}, "corr-unknown")
	require.ErrorIs(t, err, domain.ErrSupersedingDocumentNotFound)
}

// TestPgStore_DeclareRecord_InsertsOutboxEventAtomically and
// TestPgStore_SupersedeDocument_InsertsOutboxEventAtomically prove the two
// new events actually land, same discipline as Wave 1's tests.
func TestPgStore_DeclareRecord_InsertsOutboxEventAtomically(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "outbox-declare")

	_, err := s.DeclareRecord(tenantCtx(), domain.DeclareRecordParams{
		DocumentID: doc.DocumentID, DeclaredByPrincipalID: "declarer-1",
	}, "corr-outbox-declare")
	require.NoError(t, err)

	var count int
	err = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'document.record_declared'`,
		doc.DocumentID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPgStore_SupersedeDocument_InsertsOutboxEventAtomically(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	original := newDocForDeclareTest(t, s, "outbox-supersede-orig")
	replacement := newDocForDeclareTest(t, s, "outbox-supersede-new")

	_, err := s.SupersedeDocument(tenantCtx(), domain.SupersedeDocumentParams{
		DocumentID: original.DocumentID, SupersededByDocumentID: replacement.DocumentID, ActorPrincipalID: "actor-1",
	}, "corr-outbox-supersede")
	require.NoError(t, err)

	var count int
	err = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'document.superseded'`,
		original.DocumentID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

// TestPgStore_MoveToArchive_Succeeds_ThenRejectsSecondArchive is the real
// proof of Wave 3's MoveToArchive, including the negative control at the
// DB layer.
func TestPgStore_MoveToArchive_Succeeds_ThenRejectsSecondArchive(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "archive")

	archived, err := s.MoveToArchive(tenantCtx(), domain.MoveToArchiveParams{
		DocumentID: doc.DocumentID, ArchivedByPrincipalID: "archiver-1", Reason: "no longer needed",
	}, "corr-archive-1")
	require.NoError(t, err)
	require.Equal(t, domain.StatusArchived, archived.Status)
	require.NotNil(t, archived.ArchivedAt)
	require.NotNil(t, archived.ArchivedByPrincipalID)
	require.Equal(t, "archiver-1", *archived.ArchivedByPrincipalID)

	_, err = s.MoveToArchive(tenantCtx(), domain.MoveToArchiveParams{
		DocumentID: doc.DocumentID, ArchivedByPrincipalID: "archiver-2",
	}, "corr-archive-2")
	require.ErrorIs(t, err, domain.ErrDocumentNotArchivable)

	// Negative control at the DB layer.
	_, err = pool.Exec(tenantCtx(), `UPDATE documents SET archived_by_principal_id = 'tampered' WHERE document_id = $1`, doc.DocumentID)
	require.Error(t, err, "expected the trigger to refuse mutating an existing archive")

	var count int
	err = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'document.archived'`, doc.DocumentID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

// TestPgStore_MoveToArchive_SupersededDocumentIsArchivable proves the two
// non-active pre-archive paths — SUPERSEDED is a legal starting point,
// ARCHIVED itself is not.
func TestPgStore_MoveToArchive_SupersededDocumentIsArchivable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	original := newDocForDeclareTest(t, s, "archive-superseded-orig")
	replacement := newDocForDeclareTest(t, s, "archive-superseded-new")
	_, err := s.SupersedeDocument(tenantCtx(), domain.SupersedeDocumentParams{
		DocumentID: original.DocumentID, SupersededByDocumentID: replacement.DocumentID, ActorPrincipalID: "actor-1",
	}, "corr-super")
	require.NoError(t, err)

	archived, err := s.MoveToArchive(tenantCtx(), domain.MoveToArchiveParams{
		DocumentID: original.DocumentID, ArchivedByPrincipalID: "archiver-1",
	}, "corr-archive-superseded")
	require.NoError(t, err)
	require.Equal(t, domain.StatusArchived, archived.Status)
}

// TestPgStore_RequestDisposition_Succeeds_ThenRejectsSecondRequest mirrors
// the archive proof for RequestDisposition.
func TestPgStore_RequestDisposition_Succeeds_ThenRejectsSecondRequest(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "disposition")

	requested, err := s.RequestDisposition(tenantCtx(), domain.RequestDispositionParams{
		DocumentID: doc.DocumentID, RequestedByPrincipalID: "requester-1", Reason: "retention period expired",
	}, "corr-disposition-1")
	require.NoError(t, err)
	require.Equal(t, domain.StatusPurgePending, requested.Status)
	require.NotNil(t, requested.DispositionRequestedAt)
	require.NotNil(t, requested.DispositionRequestedByPrincipalID)

	_, err = s.RequestDisposition(tenantCtx(), domain.RequestDispositionParams{
		DocumentID: doc.DocumentID, RequestedByPrincipalID: "requester-2",
	}, "corr-disposition-2")
	require.ErrorIs(t, err, domain.ErrDispositionAlreadyRequested)

	// Negative control at the DB layer.
	_, err = pool.Exec(tenantCtx(), `UPDATE documents SET disposition_requested_by_principal_id = 'tampered' WHERE document_id = $1`, doc.DocumentID)
	require.Error(t, err, "expected the trigger to refuse mutating an existing disposition request")

	var count int
	err = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'document.disposition_requested'`, doc.DocumentID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

// TestPgStore_GetAsOfDocument_ReconstructsVersionLineage is the real
// proof of Wave 3's GetAsOfDocument — it reconstructs which version was
// current as of a given time from document_versions' own immutable
// timestamps.
func TestPgStore_GetAsOfDocument_ReconstructsVersionLineage(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "asof")

	var midpoint time.Time
	err := pool.QueryRow(tenantCtx(), `SELECT created_at FROM document_versions WHERE document_id = $1 AND version = 1`, doc.DocumentID).Scan(&midpoint)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("asof-v2"), StorageKey: "key-asof-v2", SizeBytes: 6,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	_, err = s.AddVersion(tenantCtx(), doc.DocumentID, v2, "corr-asof-v2")
	require.NoError(t, err)

	_, asOfMidpoint, err := s.GetAsOfDocument(tenantCtx(), doc.DocumentID, midpoint.Add(1*time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, 1, asOfMidpoint.Version, "expected version 1 to be current at the midpoint")

	_, asOfNow, err := s.GetAsOfDocument(tenantCtx(), doc.DocumentID, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, 2, asOfNow.Version, "expected version 2 to be current now")

	_, _, err = s.GetAsOfDocument(tenantCtx(), doc.DocumentID, midpoint.Add(-1*time.Hour))
	require.ErrorIs(t, err, domain.ErrDocumentVersionNotFound, "expected no version to exist before the document was even created")
}

// TestPgStore_LinkDocument_Succeeds_ThenRejectsDuplicate is the real
// proof of GetLinkedObjects' data source: a link is recorded, retrievable,
// and — the negative control — the DB's own unique constraint refuses a
// duplicate link for the same (document, object) pair.
func TestPgStore_LinkDocument_Succeeds_ThenRejectsDuplicate(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc := newDocForDeclareTest(t, s, "link")

	link, err := s.LinkDocument(tenantCtx(), domain.LinkDocumentParams{
		DocumentID: doc.DocumentID, LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "claim-123",
		LinkedByPrincipalID: "linker-1", CorrelationID: "corr-link-1",
	})
	require.NoError(t, err)
	require.NotEmpty(t, link.LinkID)
	require.Equal(t, "EXPENSE_CLAIM", link.LinkedObjectType)
	require.Equal(t, "claim-123", link.LinkedObjectID)

	_, err = s.LinkDocument(tenantCtx(), domain.LinkDocumentParams{
		DocumentID: doc.DocumentID, LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "claim-123",
		LinkedByPrincipalID: "linker-2",
	})
	require.ErrorIs(t, err, domain.ErrDuplicateLink)

	// A different object type/ID for the SAME document is a genuinely
	// different link and must succeed.
	second, err := s.LinkDocument(tenantCtx(), domain.LinkDocumentParams{
		DocumentID: doc.DocumentID, LinkedObjectType: "WORKFLOW_INSTANCE", LinkedObjectID: "wf-456",
		LinkedByPrincipalID: "linker-1",
	})
	require.NoError(t, err)

	links, err := s.ListDocumentLinks(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, links, 2)

	// Negative control at the DB layer: links are append-only.
	_, err = pool.Exec(tenantCtx(), `UPDATE document_links SET linked_object_id = 'tampered' WHERE link_id = $1`, second.LinkID)
	require.Error(t, err, "expected the trigger to refuse updating an existing link")
	_, err = pool.Exec(tenantCtx(), `DELETE FROM document_links WHERE link_id = $1`, second.LinkID)
	require.Error(t, err, "expected the trigger to refuse deleting an existing link")
}

func TestPgStore_LinkDocument_UnknownDocument_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, err := s.LinkDocument(tenantCtx(), domain.LinkDocumentParams{
		DocumentID: "00000000-0000-0000-0000-000000000000", LinkedObjectType: "EXPENSE_CLAIM", LinkedObjectID: "claim-1",
		LinkedByPrincipalID: "linker-1",
	})
	require.ErrorIs(t, err, domain.ErrDocumentNotFound)
}

func TestPgStore_AddVersion_UnknownDocument_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.AddVersion(tenantCtx(), "00000000-0000-0000-0000-000000000000",
		&domain.DocumentVersion{ChecksumSHA256: sha256Hex("x"), StorageKey: "k", ContentType: "text/plain", CreatedByPrincipalID: "p"}, "corr-notfound")
	require.ErrorIs(t, err, domain.ErrDocumentNotFound)
}

// TestPgStore_CreateDocument_InsertsOutboxEventAtomically is the real
// proof of Wave 1 (BIZ-01 events): CreateDocument must leave a
// document.uploaded row in outbox_events, written in the SAME transaction
// as the document/version insert — see internal/outbox's own package doc.
func TestPgStore_CreateDocument_InsertsOutboxEventAtomically(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Outbox Test Doc", Classification: domain.ClassificationInternal,
		CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("outbox-1"), StorageKey: "key-outbox-1", SizeBytes: 5,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-outbox-1"))

	var eventType, aggregateID, correlationID string
	var publishedAt *string
	err := pool.QueryRow(context.Background(),
		`SELECT event_type, aggregate_id, correlation_id, published_at::text FROM outbox_events WHERE aggregate_id = $1`,
		doc.DocumentID).Scan(&eventType, &aggregateID, &correlationID, &publishedAt)
	require.NoError(t, err, "expected exactly one outbox_events row for this document")
	require.Equal(t, "document.uploaded", eventType)
	require.Equal(t, doc.DocumentID, aggregateID)
	require.Equal(t, "corr-outbox-1", correlationID)
	require.Nil(t, publishedAt, "a freshly-inserted event must be unpublished until the relay picks it up")
}

// TestPgStore_AddVersion_InsertsOutboxEventAtomically mirrors the create
// test for the version_created event.
func TestPgStore_AddVersion_InsertsOutboxEventAtomically(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Outbox Version Test", Classification: domain.ClassificationInternal,
		CreatedByPrincipalID: "principal-1",
	}
	v1 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("outbox-v1"), StorageKey: "key-outbox-v1", SizeBytes: 5,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v1, "corr-outbox-v1"))

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("outbox-v2"), StorageKey: "key-outbox-v2", SizeBytes: 6,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	_, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2, "corr-outbox-v2")
	require.NoError(t, err)

	var count int
	err = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'document.version_created'`,
		doc.DocumentID).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "expected exactly one document.version_created event")
}

func TestPgStore_RecordAccess_IsAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Doc", Classification: domain.ClassificationPublic, CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("sum"), StorageKey: "key", SizeBytes: 1,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-166"))

	for i := 0; i < 3; i++ {
		require.NoError(t, s.RecordAccess(tenantCtx(), &domain.DocumentAccessLog{
			DocumentID: doc.DocumentID, AccessedByPrincipalID: "reader-1", AccessType: domain.AccessMetadata,
		}))
	}

	entries, err := s.ListAccessLog(tenantCtx(), doc.DocumentID, 100, 0)
	require.NoError(t, err)
	require.Len(t, entries, 3, "every access must be recorded, none overwritten")
}
