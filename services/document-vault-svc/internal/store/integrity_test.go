package store_test

// Real-PostgreSQL tests for: append-only document_versions / document_access_log
// (migration 000013), idempotent document and version commands (000014) and the
// legal-hold check on disposition requests. Skipped without TEST_DATABASE_URL,
// like the rest of this package.

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/store"
)

const integrityAppRole = "document_vault_test_app"

// appRolePool connects as a NOSUPERUSER NOBYPASSRLS role holding the strongest
// privileges the application could ever have on the tables, so the only thing
// standing between it and a mutation is the trigger (or RLS).
func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)

	_, err = admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+integrityAppRole+`') THEN
			CREATE ROLE `+integrityAppRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`)
	require.NoError(t, err)
	for _, stmt := range []string{
		`ALTER ROLE ` + integrityAppRole + ` WITH LOGIN PASSWORD 'app_test_pw' NOSUPERUSER NOBYPASSRLS`,
		`GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO ` + integrityAppRole,
	} {
		_, err := admin.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	dsn.User = url.UserPassword(integrityAppRole, "app_test_pw")
	pool, err := pgxpool.New(ctx, dsn.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var super, bypass bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass))
	require.False(t, super || bypass, "the application-role assertions are meaningless over a privileged connection")
	return pool
}

func newDoc(title string) (*domain.Document, *domain.DocumentVersion) {
	return &domain.Document{
		TenantID: testTenantID, LegalEntityID: "22222222-2222-2222-2222-222222222222",
		Title: title, Classification: domain.ClassificationConfidential, RetentionPolicy: "DEFAULT",
		CreatedByPrincipalID: "principal-1",
	}, &domain.DocumentVersion{
		ChecksumSHA256: sha256Hex("blob-" + title), StorageKey: "key-" + title, SizeBytes: 10,
		ContentType: "text/plain", CreatedByPrincipalID: "principal-1",
	}
}

func countDocuments(t *testing.T, admin *pgxpool.Pool, title string) int {
	t.Helper()
	var n int
	require.NoError(t, admin.QueryRow(context.Background(), `SELECT COUNT(*) FROM documents WHERE title = $1`, title).Scan(&n))
	return n
}

func TestDocumentVersionsAndAccessLog_AreAppendOnly(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	app := appRolePool(t, admin)
	ctx := tenantCtx()

	// INSERT keeps working: a document, an appended version, an access-log entry.
	doc, v1 := newDoc("append-only")
	require.NoError(t, s.CreateDocument(ctx, doc, v1, "corr-ao-1"))
	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v2"), StorageKey: "key-v2", SizeBytes: 5, ContentType: "text/plain", CreatedByPrincipalID: "principal-1"}
	_, err := s.AddVersion(ctx, doc.DocumentID, v2, "corr-ao-2")
	require.NoError(t, err)
	require.NoError(t, s.RecordAccess(ctx, &domain.DocumentAccessLog{
		DocumentID: doc.DocumentID, DocumentVersionID: &v1.DocumentVersionID,
		AccessedByPrincipalID: "reader-1", AccessType: domain.AccessDownload,
	}))

	stmts := []string{
		`UPDATE document_versions SET checksum_sha256 = '` + strings.Repeat("a", 64) + `' WHERE document_version_id = '` + v1.DocumentVersionID + `'`,
		`UPDATE document_versions SET storage_key = 'elsewhere'`,
		`DELETE FROM document_versions WHERE document_version_id = '` + v1.DocumentVersionID + `'`,
		`TRUNCATE document_versions CASCADE`,
		`UPDATE document_access_log SET accessed_by_principal_id = 'someone-else'`,
		`DELETE FROM document_access_log`,
		`TRUNCATE document_access_log`,
	}

	// Refused for a superuser...
	for _, stmt := range stmts {
		_, err := admin.Exec(context.Background(), stmt)
		require.Errorf(t, err, "superuser must be refused: %s", stmt)
		assert.Contains(t, err.Error(), "append-only", stmt)
	}
	// ...and for the application role holding UPDATE/DELETE/TRUNCATE, in a
	// tenant-scoped session (the only way RLS lets it see the rows at all).
	for _, stmt := range stmts {
		tx, err := app.Begin(context.Background())
		require.NoError(t, err)
		_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, testTenantID)
		require.NoError(t, err)
		_, err = tx.Exec(context.Background(), stmt)
		_ = tx.Rollback(context.Background())
		require.Errorf(t, err, "application role must be refused: %s", stmt)
		assert.Contains(t, err.Error(), "append-only", stmt)
	}

	// Nothing changed; reads work.
	versions, err := s.ListVersions(ctx, doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, sha256Hex("blob-append-only"), versions[0].ChecksumSHA256)
	log, err := s.ListAccessLog(ctx, doc.DocumentID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, log, 1)

	// Tenant isolation is unchanged: the application role with no tenant
	// context still sees none of it (FORCE RLS), and another tenant sees none.
	var n int
	require.NoError(t, app.QueryRow(context.Background(), `SELECT COUNT(*) FROM document_versions`).Scan(&n))
	assert.Equal(t, 0, n)
	otherTenant := middleware.WithTenant(context.Background(), "99999999-9999-9999-9999-999999999999")
	_, err = s.FindDocumentByID(otherTenant, doc.DocumentID)
	require.ErrorIs(t, err, domain.ErrDocumentNotFound)
}

func claim(principal, key, hash string) domain.IdempotencyClaim {
	return domain.IdempotencyClaim{PrincipalID: principal, Key: key, Operation: "CreateDocument", RequestHash: hash}
}

func TestCreateDocumentIdempotent_SameKeyReplays_OneDocument(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	ctx := tenantCtx()
	h := strings.Repeat("a", 64)

	doc1, v1 := newDoc("idem-replay")
	existing, err := s.CreateDocumentIdempotent(ctx, claim("p1", "key-1", h), doc1, v1, "corr-1")
	require.NoError(t, err)
	require.Nil(t, existing, "first use of a key performs the operation")
	require.NotEmpty(t, doc1.DocumentID)

	// Retry: nothing is created, the stored outcome comes back.
	doc2, v2 := newDoc("idem-replay")
	existing, err = s.CreateDocumentIdempotent(ctx, claim("p1", "key-1", h), doc2, v2, "corr-2")
	require.NoError(t, err)
	require.NotNil(t, existing)
	assert.Equal(t, h, existing.RequestHash)
	assert.Equal(t, 201, existing.ResponseStatus)
	assert.Contains(t, string(existing.ResponseBody), doc1.DocumentID, "the replay carries the ORIGINAL document")
	assert.Empty(t, doc2.DocumentID, "the retry created nothing")
	assert.Equal(t, 1, countDocuments(t, admin, "idem-replay"))

	got, err := s.GetIdempotency(ctx, "p1", "key-1")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestCreateDocumentIdempotent_ScopeIsTenantPrincipalKey(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	ctx := tenantCtx()
	h := strings.Repeat("b", 64)

	d1, v1 := newDoc("idem-scope")
	_, err := s.CreateDocumentIdempotent(ctx, claim("p1", "shared-key", h), d1, v1, "c1")
	require.NoError(t, err)

	// Same key, DIFFERENT principal: independent — never replays p1's response.
	d2, v2 := newDoc("idem-scope")
	existing, err := s.CreateDocumentIdempotent(ctx, claim("p2", "shared-key", h), d2, v2, "c2")
	require.NoError(t, err)
	require.Nil(t, existing)

	// Different key, same principal: independent.
	d3, v3 := newDoc("idem-scope")
	existing, err = s.CreateDocumentIdempotent(ctx, claim("p1", "other-key", h), d3, v3, "c3")
	require.NoError(t, err)
	require.Nil(t, existing)

	assert.Equal(t, 3, countDocuments(t, admin, "idem-scope"))

	// Another tenant cannot see the key at all.
	otherTenant := middleware.WithTenant(context.Background(), "99999999-9999-9999-9999-999999999999")
	rec, err := s.GetIdempotency(otherTenant, "p1", "shared-key")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestCreateDocumentIdempotent_ConcurrentSameKey_OneLogicalOperation(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	ctx := tenantCtx()
	h := strings.Repeat("c", 64)

	const n = 8
	var wg sync.WaitGroup
	creators := make(chan struct{}, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, v := newDoc("idem-race")
			existing, err := s.CreateDocumentIdempotent(ctx, claim("p1", "race-key", h), doc, v, "c")
			if err != nil {
				errs <- err
				return
			}
			if existing == nil {
				creators <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(creators)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, len(creators), "exactly one request performed the operation")
	assert.Equal(t, 1, countDocuments(t, admin, "idem-race"), "the losers' transactions rolled back")
}

func TestCreateDocumentIdempotent_FailedOperationClaimsNothing(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	ctx := tenantCtx()
	h := strings.Repeat("d", 64)

	doc, v := newDoc("idem-fail")
	doc.Classification = "NOT_A_CLASSIFICATION" // violates the CHECK constraint
	_, err := s.CreateDocumentIdempotent(ctx, claim("p1", "fail-key", h), doc, v, "c")
	require.Error(t, err)

	rec, err := s.GetIdempotency(ctx, "p1", "fail-key")
	require.NoError(t, err)
	assert.Nil(t, rec, "a failed operation must not consume the key")

	// The same key is then usable.
	doc2, v2 := newDoc("idem-fail")
	existing, err := s.CreateDocumentIdempotent(ctx, claim("p1", "fail-key", h), doc2, v2, "c")
	require.NoError(t, err)
	assert.Nil(t, existing)
}

func TestAddVersionIdempotent_SameKeyReplays_OneVersion(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	ctx := tenantCtx()

	doc, v1 := newDoc("idem-version")
	require.NoError(t, s.CreateDocument(ctx, doc, v1, "c0"))
	h := strings.Repeat("e", 64)
	c := domain.IdempotencyClaim{PrincipalID: "p1", Key: "ver-key", Operation: "AddVersion", RequestHash: h}

	v2 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v2-idem"), StorageKey: "k2", SizeBytes: 3, ContentType: "text/plain", CreatedByPrincipalID: "p1"}
	out, existing, err := s.AddVersionIdempotent(ctx, c, doc.DocumentID, v2, "c1")
	require.NoError(t, err)
	require.Nil(t, existing)
	require.Equal(t, 2, out.CurrentVersion)

	v3 := &domain.DocumentVersion{ChecksumSHA256: sha256Hex("v3-idem"), StorageKey: "k3", SizeBytes: 3, ContentType: "text/plain", CreatedByPrincipalID: "p1"}
	out2, existing, err := s.AddVersionIdempotent(ctx, c, doc.DocumentID, v3, "c2")
	require.NoError(t, err)
	require.NotNil(t, existing)
	assert.Nil(t, out2)

	versions, err := s.ListVersions(ctx, doc.DocumentID)
	require.NoError(t, err)
	assert.Len(t, versions, 2, "the retry appended no third version")
}

// DRC-I10 / DRC-008: a document whose declared record is under an active legal
// hold cannot be put into disposition; releasing the hold unblocks it.
func TestRequestDisposition_BlockedWhileARecordOfTheDocumentIsUnderHold(t *testing.T) {
	admin := requireTestDB(t)
	s := store.New(admin, zap.NewNop())
	ctx := tenantCtx()

	rec := createTestRecord(t, s, "disp-hold", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold, err := s.CreateLegalHold(ctx, domain.CreateLegalHoldParams{
		MatterRef: "Matter-disp", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)

	// A document with no held record is unaffected by someone else's hold.
	probe, probeV := newDoc("disp-free")
	require.NoError(t, s.CreateDocument(ctx, probe, probeV, "c"))
	_, err = s.RequestDisposition(ctx, domain.RequestDispositionParams{DocumentID: probe.DocumentID, RequestedByPrincipalID: "ops-1"}, "c")
	require.NoError(t, err, "a document with no held record is not blocked")

	_, err = s.ActivateLegalHold(ctx, domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)

	_, err = s.RequestDisposition(ctx, domain.RequestDispositionParams{DocumentID: rec.DocumentID, RequestedByPrincipalID: "ops-1", Reason: "cleanup"}, "c2")
	require.ErrorIs(t, err, domain.ErrRecordUnderLegalHold)
	assert.Contains(t, err.Error(), hold.HoldID, "the block names the hold")

	doc, err := s.FindDocumentByID(ctx, rec.DocumentID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, doc.Status, "nothing was mutated")
	assert.Nil(t, doc.DispositionRequestedAt)

	_, err = s.ReleaseLegalHold(ctx, domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "resolved", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "director-dana",
	})
	require.NoError(t, err)
	released, err := s.RequestDisposition(ctx, domain.RequestDispositionParams{DocumentID: rec.DocumentID, RequestedByPrincipalID: "ops-1"}, "c3")
	require.NoError(t, err, "released hold no longer blocks")
	assert.Equal(t, domain.StatusPurgePending, released.Status)
}
