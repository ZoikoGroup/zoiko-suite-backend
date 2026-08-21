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

	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v))
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
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v1))

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v2sum"), StorageKey: "key-2", SizeBytes: 20,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-2"}
	updated, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2)
	require.NoError(t, err)
	require.Equal(t, 2, updated.CurrentVersion)

	versions, err := s.ListVersions(tenantCtx(), doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, versions, 2, "both versions must remain — lineage is append-only, never overwritten")
	require.Equal(t, sha256Hex("v1sum"), versions[0].ChecksumSHA256)
	require.Equal(t, sha256Hex("v2sum"), versions[1].ChecksumSHA256)
}

func TestPgStore_AddVersion_UnknownDocument_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.AddVersion(tenantCtx(), "00000000-0000-0000-0000-000000000000",
		&domain.DocumentVersion{ChecksumSHA256: sha256Hex("x"), StorageKey: "k", ContentType: "text/plain", CreatedByPrincipalID: "p"})
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
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("sum"), StorageKey: "key", SizeBytes: 1,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v))

	for i := 0; i < 3; i++ {
		require.NoError(t, s.RecordAccess(tenantCtx(), &domain.DocumentAccessLog{
			DocumentID: doc.DocumentID, AccessedByPrincipalID: "reader-1", AccessType: domain.AccessMetadata,
		}))
	}

	entries, err := s.ListAccessLog(tenantCtx(), doc.DocumentID, 100, 0)
	require.NoError(t, err)
	require.Len(t, entries, 3, "every access must be recorded, none overwritten")
}
