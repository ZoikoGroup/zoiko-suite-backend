package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/store"
)

// GET /v1/document-versions/{version_id} (ZS-SVC-S-001 §3.6) against a real
// database: exact lookup by version identity, never "latest", and tenant-scoped.

func TestPgStore_FindVersionByID_ReturnsTheExactVersion(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())

	doc := &domain.Document{
		TenantID: testTenantID, LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title: "MSA", Classification: domain.ClassificationConfidential,
		RetentionPolicy: "7_YEARS", CreatedByPrincipalID: "principal-1",
	}
	v1 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("one"), StorageKey: "k1", SizeBytes: 3,
		ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v1, "corr-v1"))
	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("two"), StorageKey: "k2", SizeBytes: 3,
		ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}
	_, err := s.AddVersion(tenantCtx(), doc.DocumentID, v2, "corr-v2")
	require.NoError(t, err)
	require.NotEqual(t, v1.DocumentVersionID, v2.DocumentVersionID)

	got1, err := s.FindVersionByID(tenantCtx(), v1.DocumentVersionID)
	require.NoError(t, err)
	require.Equal(t, v1.DocumentVersionID, got1.DocumentVersionID)
	require.Equal(t, doc.DocumentID, got1.DocumentID)
	require.Equal(t, 1, got1.Version)
	require.Equal(t, sha256Hex("one"), got1.ChecksumSHA256, "the OLDER version is returned, not the latest")

	got2, err := s.FindVersionByID(tenantCtx(), v2.DocumentVersionID)
	require.NoError(t, err)
	require.Equal(t, 2, got2.Version)
	require.Equal(t, sha256Hex("two"), got2.ChecksumSHA256)
}

func TestPgStore_FindVersionByID_IsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())

	doc := &domain.Document{
		TenantID: testTenantID, LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title: "Tenant A only", Classification: domain.ClassificationConfidential,
		RetentionPolicy: "7_YEARS", CreatedByPrincipalID: "principal-1",
	}
	v := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("secret"), StorageKey: "ka", SizeBytes: 6,
		ContentType: "application/pdf", CreatedByPrincipalID: "principal-1"}
	require.NoError(t, s.CreateDocument(tenantCtx(), doc, v, "corr-a"))

	other := middleware.WithTenant(tenantCtx(), "99999999-9999-9999-9999-999999999999")
	got, err := s.FindVersionByID(other, v.DocumentVersionID)
	require.ErrorIs(t, err, domain.ErrDocumentVersionNotFound, "another tenant sees the version as nonexistent")
	require.Nil(t, got)

	// The same id for its own tenant still resolves.
	own, err := s.FindVersionByID(tenantCtx(), v.DocumentVersionID)
	require.NoError(t, err)
	require.Equal(t, v.DocumentVersionID, own.DocumentVersionID)
}

func TestPgStore_FindVersionByID_UnknownAndMalformedIDsAreNotFound(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	for _, id := range []string{
		"00000000-0000-0000-0000-000000000000", // well formed, no such version
		"not-a-uuid", "", "latest", "1", "' OR 1=1 --",
	} {
		got, err := s.FindVersionByID(tenantCtx(), id)
		require.ErrorIs(t, err, domain.ErrDocumentVersionNotFound, "id %q", id)
		require.Nil(t, got)
	}
}

func TestPgStore_FindVersionByID_RequiresATenant(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	_, err := s.FindVersionByID(middleware.WithTenant(tenantCtx(), ""), "00000000-0000-0000-0000-000000000000")
	require.Error(t, err, "no tenant context is never an unscoped read")
}
