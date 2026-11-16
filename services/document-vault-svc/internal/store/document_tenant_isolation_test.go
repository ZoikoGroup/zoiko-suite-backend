package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// BIZ-01 implementation gate: tenant isolation for the core document store.
// Tenant B must not be able to read, list, version, link, declare, supersede,
// archive or request disposition of tenant A's document, and nothing B attempts
// may change A's data. The store runs as a NOSUPERUSER NOBYPASSRLS role so row
// level security, not a superuser bypass, is what is being exercised.
func TestPgStore_Documents_TenantIsolation(t *testing.T) {
	admin := requireTestDB(t)
	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())
	ctxA, ctxB := tenantCtx(), otherTenantCtx()

	docA, vA1 := newDoc("iso-a")
	require.NoError(t, s.CreateDocument(ctxA, docA, vA1, "corr-iso-1"))
	vA2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("iso-a-v2"), StorageKey: "key-iso-a-v2", SizeBytes: 5, ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	_, err := s.AddVersion(ctxA, docA.DocumentID, vA2, "corr-iso-2")
	require.NoError(t, err)
	successor, vS := newDoc("iso-a-successor")
	require.NoError(t, s.CreateDocument(ctxA, successor, vS, "corr-iso-3"))
	_, err = s.LinkDocument(ctxA, domain.LinkDocumentParams{DocumentID: docA.DocumentID, LinkedObjectType: "INVOICE", LinkedObjectID: "inv-1", LinkedByPrincipalID: "principal-1", CorrelationID: "corr-iso-4"})
	require.NoError(t, err)

	// Reads: B sees nothing of A's.
	_, err = s.FindDocumentByID(ctxB, docA.DocumentID)
	require.ErrorIs(t, err, domain.ErrDocumentNotFound)
	list, err := s.ListDocuments(ctxB, docA.LegalEntityID, 50, 0)
	require.NoError(t, err)
	require.Empty(t, list, "B listing the same legal-entity id must not see A's documents")
	versions, err := s.ListVersions(ctxB, docA.DocumentID)
	if err == nil {
		require.Empty(t, versions)
	} else {
		require.ErrorIs(t, err, domain.ErrDocumentNotFound)
	}
	links, err := s.ListDocumentLinks(ctxB, docA.DocumentID)
	if err == nil {
		require.Empty(t, links)
	} else {
		require.ErrorIs(t, err, domain.ErrDocumentNotFound)
	}
	_, _, err = s.GetAsOfDocument(ctxB, docA.DocumentID, time.Now().Add(time.Hour))
	require.Error(t, err, "as-of read of another tenant's document must fail")
	accessLog, err := s.ListAccessLog(ctxB, docA.DocumentID, 50, 0)
	if err == nil {
		require.Empty(t, accessLog)
	}

	// Writes: B cannot touch A's document.
	vB := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("injected"), StorageKey: "key-injected", SizeBytes: 3, ContentType: "text/plain", CreatedByPrincipalID: "attacker"}
	_, err = s.AddVersion(ctxB, docA.DocumentID, vB, "corr-iso-b1")
	require.Error(t, err, "AddVersion on another tenant's document")
	_, err = s.LinkDocument(ctxB, domain.LinkDocumentParams{DocumentID: docA.DocumentID, LinkedObjectType: "INVOICE", LinkedObjectID: "inv-x", LinkedByPrincipalID: "attacker", CorrelationID: "corr-iso-b2"})
	require.Error(t, err, "LinkDocument on another tenant's document")
	_, err = s.DeclareRecord(ctxB, domain.DeclareRecordParams{DocumentID: docA.DocumentID, DeclaredByPrincipalID: "attacker"}, "corr-iso-b3")
	require.Error(t, err, "DeclareRecord on another tenant's document")
	_, err = s.SupersedeDocument(ctxB, domain.SupersedeDocumentParams{DocumentID: docA.DocumentID, SupersededByDocumentID: successor.DocumentID, ActorPrincipalID: "attacker"}, "corr-iso-b4")
	require.Error(t, err, "SupersedeDocument on another tenant's document")
	_, err = s.MoveToArchive(ctxB, domain.MoveToArchiveParams{DocumentID: docA.DocumentID, ArchivedByPrincipalID: "attacker", Reason: "x"}, "corr-iso-b5")
	require.Error(t, err, "MoveToArchive on another tenant's document")
	_, err = s.RequestDisposition(ctxB, domain.RequestDispositionParams{DocumentID: docA.DocumentID, RequestedByPrincipalID: "attacker", Reason: "x"}, "corr-iso-b6")
	require.Error(t, err, "RequestDisposition on another tenant's document")

	// A's data is unchanged by everything B attempted.
	got, err := s.FindDocumentByID(ctxA, docA.DocumentID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusActive, got.Status, "A's document must still be ACTIVE")
	aVersions, err := s.ListVersions(ctxA, docA.DocumentID)
	require.NoError(t, err)
	require.Len(t, aVersions, 2, "A must still have exactly its two versions")
	aLinks, err := s.ListDocumentLinks(ctxA, docA.DocumentID)
	require.NoError(t, err)
	require.Len(t, aLinks, 1, "A must still have exactly its one link")

	// RLS itself, bypassing the store's own tenant predicates: with B's tenant
	// context and no WHERE tenant_id, A's rows must still be invisible.
	ctx := context.Background()
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, otherTenantID)
	require.NoError(t, err)
	for _, table := range []string{"documents", "document_versions", "document_links", "document_access_log"} {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n), table)
		require.Zero(t, n, "RLS leak: tenant B context sees %d rows in %s", n, table)
	}
}
