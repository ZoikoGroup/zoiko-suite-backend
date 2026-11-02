package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	authzpkg "zoiko.io/mtls-management-svc/internal/authz"
	internalca "zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/idempotency"
	"zoiko.io/mtls-management-svc/internal/siem"
	"zoiko.io/mtls-management-svc/internal/store"
)

// testSchema isolates this package's tables in their own schema, so packages
// that `go test ./...` runs in parallel against one database never reset each
// other's tables mid-test.
const testSchema = "mtls_handler_test"

func schemaDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + testSchema
}

// End-to-end through the real router on Postgres: the production wiring
// (PgStore + idempotency.PgStore), with connections running as an ordinary
// NOSUPERUSER NOBYPASSRLS role (TEST_RLS_ROLE). Skips without TEST_DATABASE_URL;
// see internal/store/pg_store_test.go for the variables.

// policyAuthz grants exactly the (principal, action) pairs it holds.
type policyAuthz struct{ grants map[string]bool }

func (p policyAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if p.grants[principal+"|"+action] {
		return nil
	}
	return authzpkg.ErrAuthorizationDenied
}

func allActions(principal string) map[string]bool {
	g := map[string]bool{}
	for _, a := range []string{handler.MtlsCertProvision, handler.MtlsCertRead, handler.MtlsCertRotate,
		handler.MtlsCertRevoke, handler.MtlsPolicyWrite, handler.MtlsPolicyRead} {
		g[principal+"|"+a] = true
	}
	return g
}

type pgEnv struct {
	admin *pgxpool.Pool
	ca    *internalca.CA
	l     *internalca.Ledger
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	u, _ := url.Parse(dsn)
	if db := strings.ToLower(strings.TrimPrefix(u.Path, "/")); !strings.Contains(db, "test") && !strings.Contains(db, "b810") {
		t.Fatalf("refusing to run against %q", db)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, schemaDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "deployments", "migrations")
	downs, _ := filepath.Glob(filepath.Join(dir, "*.down.sql"))
	ups, _ := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if _, err := admin.Exec(context.Background(), `CREATE SCHEMA IF NOT EXISTS `+testSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(downs)))
	sort.Strings(ups)
	for _, f := range append(downs, ups...) {
		sql, _ := os.ReadFile(f)
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
	}
	role := pgx.Identifier{testRLSRole()}.Sanitize()
	lit := "'" + strings.ReplaceAll(testRLSRole(), "'", "''") + "'"
	for _, stmt := range []string{
		fmt.Sprintf(`DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF; END$$`, lit, role),
		`GRANT USAGE ON SCHEMA ` + testSchema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON mtls_certificates, mtls_communication_policies, idempotency_keys TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	c, err := internalca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &pgEnv{admin: admin, ca: c, l: mustLedger(t)}
}

func testRLSRole() string {
	if r := os.Getenv("TEST_RLS_ROLE"); r != "" {
		return r
	}
	return "app_rls_probe"
}

// router builds a fresh process: new pool, new stores, same CA and ledger.
func (e *pgEnv) router(t *testing.T, az handler.AuthzChecker) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	cfg, _ := pgxpool.ParseConfig(schemaDSN(os.Getenv("TEST_DATABASE_URL")))
	role := pgx.Identifier{testRLSRole()}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := handler.New(store.NewPgStore(pool), e.ca, e.l, siem.New("", "mtls-management-svc", zap.NewNop()), az, zap.NewNop(), nil).
		WithIdempotency(idempotency.NewPgStore(pool))
	return handler.NewRouter(h), pool
}

func call(r http.Handler, method, path, tenant, principal, key string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if tenant != "" && principal != "" {
		withEnvelope(req)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var provisionBody = map[string]any{"legal_entity_id": "le-1", "service_name": "svc-a", "common_name": "svc-a", "rotation_days": 30}

func certID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var res struct {
		Certificate struct{ ID string } `json:"certificate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Certificate.ID == "" {
		t.Fatalf("no certificate id in %d %s", w.Code, w.Body)
	}
	return res.Certificate.ID
}

func TestPgRouter_LifecyclePersistsAcrossRestart(t *testing.T) {
	e := newPgEnv(t)
	az := policyAuthz{allActions("alice")}
	r1, _ := e.router(t, az)

	w := call(r1, http.MethodPost, "/v1/mtls/certificates", "t1", "alice", "", provisionBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision: %d %s", w.Code, w.Body)
	}
	id := certID(t, w)
	if w := call(r1, http.MethodPost, "/v1/mtls/certificates/"+id+"/rotate", "t1", "alice", "", nil); w.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}

	r2, _ := e.router(t, az) // restart
	w = call(r2, http.MethodGet, "/v1/mtls/certificates/"+id, "t1", "alice", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("certificate lost across restart: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"prior_serial_numbers":[`) || strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatalf("restart read must keep rotation history and never carry a key: %s", w.Body)
	}
	if w := call(r2, http.MethodDelete, "/v1/mtls/certificates/"+id, "t1", "alice", "", nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	r3, _ := e.router(t, az)
	if w := call(r3, http.MethodPost, "/v1/mtls/certificates/"+id+"/rotate", "t1", "alice", "", nil); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "certificate_revoked") {
		t.Fatalf("revocation must survive restart and refuse rotation: %d %s", w.Code, w.Body)
	}
}

func TestPgRouter_CrossTenantIs404(t *testing.T) {
	e := newPgEnv(t)
	r, _ := e.router(t, policyAuthz{allActions("alice")})
	id := certID(t, call(r, http.MethodPost, "/v1/mtls/certificates", "tenant-a", "alice", "", provisionBody))
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/mtls/certificates/" + id},
		{http.MethodPost, "/v1/mtls/certificates/" + id + "/rotate"},
		{http.MethodDelete, "/v1/mtls/certificates/" + id},
	} {
		if w := call(r, tc.method, tc.path, "tenant-b", "alice", "", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s %s from another tenant: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	w := call(r, http.MethodGet, "/v1/mtls/certificates/"+id, "tenant-a", "alice", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ACTIVE"`) {
		t.Fatalf("owner's certificate disturbed: %d %s", w.Code, w.Body)
	}
}

func TestPgRouter_AuthzAllowAndDeny(t *testing.T) {
	e := newPgEnv(t)
	grants := allActions("alice")
	grants["bob|"+handler.MtlsCertRead] = true // bob may read, not revoke
	r, _ := e.router(t, policyAuthz{grants})
	id := certID(t, call(r, http.MethodPost, "/v1/mtls/certificates", "t1", "alice", "", provisionBody))

	if w := call(r, http.MethodGet, "/v1/mtls/certificates/"+id, "t1", "bob", "", nil); w.Code != http.StatusOK {
		t.Fatalf("granted read: %d %s", w.Code, w.Body)
	}
	if w := call(r, http.MethodDelete, "/v1/mtls/certificates/"+id, "t1", "bob", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted revoke: %d %s", w.Code, w.Body)
	}
	if w := call(r, http.MethodPost, "/v1/mtls/certificates", "t1", "mallory", "", provisionBody); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted provision: %d %s", w.Code, w.Body)
	}
	w := call(r, http.MethodGet, "/v1/mtls/certificates/"+id, "t1", "alice", "", nil)
	if !strings.Contains(w.Body.String(), `"status":"ACTIVE"`) {
		t.Fatalf("a denied revoke changed the record: %s", w.Body)
	}
	var n int
	_ = e.admin.QueryRow(context.Background(), `SELECT count(*) FROM mtls_certificates`).Scan(&n)
	if n != 1 {
		t.Fatalf("a denied provision persisted a record: %d rows", n)
	}
}

func TestPgRouter_MissingTenantOrPrincipal(t *testing.T) {
	e := newPgEnv(t)
	r, _ := e.router(t, policyAuthz{allActions("alice")})
	if w := call(r, http.MethodGet, "/v1/mtls/certificates?legal_entity_id=le-1", "", "alice", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d %s", w.Code, w.Body)
	}
	if w := call(r, http.MethodGet, "/v1/mtls/certificates?legal_entity_id=le-1", "t1", "", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: %d %s", w.Code, w.Body)
	}
}

// A retried provision with the same Idempotency-Key does not mint a second
// certificate, and the private key is neither replayed nor stored.
func TestPgRouter_DuplicateProvisionIsNotReissued(t *testing.T) {
	e := newPgEnv(t)
	r, _ := e.router(t, policyAuthz{allActions("alice")})
	first := call(r, http.MethodPost, "/v1/mtls/certificates", "t1", "alice", "dup-1", provisionBody)
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), "PRIVATE KEY") {
		t.Fatalf("first provision: %d", first.Code)
	}
	second := call(r, http.MethodPost, "/v1/mtls/certificates", "t1", "alice", "dup-1", provisionBody)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "idempotency_secret_not_replayable") ||
		strings.Contains(second.Body.String(), "PRIVATE KEY") {
		t.Fatalf("replay: %d %s", second.Code, second.Body)
	}
	ctx := context.Background()
	var certs, leaked int
	_ = e.admin.QueryRow(ctx, `SELECT count(*) FROM mtls_certificates`).Scan(&certs)
	_ = e.admin.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE position('PRIVATE KEY' in convert_from(coalesce(response_body, ''::bytea), 'UTF8')) > 0`).Scan(&leaked)
	if certs != 1 || leaked != 0 {
		t.Fatalf("certs=%d (want 1), idempotency rows holding a private key=%d (want 0)", certs, leaked)
	}

	// A policy create has no secret: its replay is the first answer verbatim.
	pol := map[string]any{"policy_name": "p", "source_service": "a", "target_service": "b", "action": "ALLOW"}
	p1 := call(r, http.MethodPost, "/v1/mtls/policies", "t1", "alice", "pol-1", pol)
	p2 := call(r, http.MethodPost, "/v1/mtls/policies", "t1", "alice", "pol-1", pol)
	if p1.Code != http.StatusCreated || p2.Code != http.StatusCreated || p1.Body.String() != p2.Body.String() ||
		p2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("policy replay: %d %s / %d %s", p1.Code, p1.Body, p2.Code, p2.Body)
	}
	var pols int
	_ = e.admin.QueryRow(ctx, `SELECT count(*) FROM mtls_communication_policies`).Scan(&pols)
	if pols != 1 {
		t.Fatalf("duplicate request created %d policies", pols)
	}
}

func TestPgRouter_ReadyzFollowsTheDatabase(t *testing.T) {
	e := newPgEnv(t)
	r, pool := e.router(t, policyAuthz{})
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil)) // no identity headers at all
		return w
	}
	if w := get("/healthz"); w.Code != http.StatusOK {
		t.Fatalf("healthz: %d %s", w.Code, w.Body)
	}
	if w := get("/readyz"); w.Code != http.StatusOK {
		t.Fatalf("readyz with database up: %d %s", w.Code, w.Body)
	}
	pool.Close()
	if w := get("/readyz"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("readyz with database gone: %d %s", w.Code, w.Body)
	}
}

// Not Postgres-specific: readiness fails closed on any store error.
func TestReadyz_UnavailableStoreIs503(t *testing.T) {
	c, err := internalca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := handler.NewRouter(handler.New(downStore{store.NewMemoryStore()}, c, mustLedger(t),
		siem.New("", "mtls-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop(), nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz must not depend on the store: %d", w.Code)
	}
}

type downStore struct{ *store.MemoryStore }

func (downStore) Ping(context.Context) error { return errors.New("connection refused") }
