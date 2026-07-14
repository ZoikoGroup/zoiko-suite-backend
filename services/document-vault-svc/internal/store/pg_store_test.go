package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
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
		DROP TABLE IF EXISTS document_access_log;
		DROP TABLE IF EXISTS document_versions;
		DROP TABLE IF EXISTS documents;
	`)
	require.NoError(t, err)

	sql, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(sql))
	require.NoError(t, err)

	return pool
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
	v := &domain.DocumentVersion{ChecksumSHA256: "abc123", StorageKey: "key-1", SizeBytes: 100,
		ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}

	require.NoError(t, s.CreateDocument(context.Background(), doc, v))
	require.NotEmpty(t, doc.DocumentID)
	require.Equal(t, 1, doc.CurrentVersion)
	require.NotEmpty(t, v.DocumentVersionID)

	found, err := s.FindDocumentByID(context.Background(), doc.DocumentID)
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
	v1 := &domain.DocumentVersion{ChecksumSHA256: "v1sum", StorageKey: "key-1", SizeBytes: 10,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(context.Background(), doc, v1))

	v2 := &domain.DocumentVersion{ChecksumSHA256: "v2sum", StorageKey: "key-2", SizeBytes: 20,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-2"}
	updated, err := s.AddVersion(context.Background(), doc.DocumentID, v2)
	require.NoError(t, err)
	require.Equal(t, 2, updated.CurrentVersion)

	versions, err := s.ListVersions(context.Background(), doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, versions, 2, "both versions must remain — lineage is append-only, never overwritten")
	require.Equal(t, "v1sum", versions[0].ChecksumSHA256)
	require.Equal(t, "v2sum", versions[1].ChecksumSHA256)
}

func TestPgStore_AddVersion_UnknownDocument_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.AddVersion(context.Background(), "00000000-0000-0000-0000-000000000000",
		&domain.DocumentVersion{ChecksumSHA256: "x", StorageKey: "k", ContentType: "text/plain", CreatedByPrincipalID: "p"})
	require.ErrorIs(t, err, domain.ErrDocumentNotFound)
}

func TestPgStore_RecordAccess_IsAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	doc := &domain.Document{
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title:         "Doc", Classification: domain.ClassificationPublic, CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: "sum", StorageKey: "key", SizeBytes: 1,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(context.Background(), doc, v))

	for i := 0; i < 3; i++ {
		require.NoError(t, s.RecordAccess(context.Background(), &domain.DocumentAccessLog{
			DocumentID: doc.DocumentID, AccessedByPrincipalID: "reader-1", AccessType: domain.AccessMetadata,
		}))
	}

	entries, err := s.ListAccessLog(context.Background(), doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, entries, 3, "every access must be recorded, none overwritten")
}
