package handler_test

import (
	"bytes"
	"context"
	"encoding/base64"
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

	authzpkg "zoiko.io/siem-integration-svc/internal/authz"
	"zoiko.io/siem-integration-svc/internal/credential"
	"zoiko.io/siem-integration-svc/internal/handler"
	"zoiko.io/siem-integration-svc/internal/idempotency"
	"zoiko.io/siem-integration-svc/internal/store"
)

// End-to-end through the router on the production wiring (PgStore with a
// credential keyring + idempotency.PgStore), every connection running as a
// NOSUPERUSER NOBYPASSRLS role. Skips without TEST_DATABASE_URL; see
// internal/store/pg_store_test.go for TEST_RLS_ROLE.

const pgTestSchema = "siem_handler_test"

func pgSchemaDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + pgTestSchema
}

func pgRLSRole() string {
	if r := os.Getenv("TEST_RLS_ROLE"); r != "" {
		return r
	}
	return "app_rls_probe"
}

// policyAuthz grants exactly the (principal, action) pairs it holds.
type policyAuthz struct{ grants map[string]bool }

func (p policyAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if p.grants[principal+"|"+action] {
		return nil
	}
	return authzpkg.ErrAuthorizationDenied
}

func grantAll(principal string) map[string]bool {
	return map[string]bool{
		principal + "|" + handler.SiemExporterCreate: true,
		principal + "|" + handler.SiemExporterRead:   true,
		principal + "|" + handler.SiemEventRead:      true,
	}
}

func pgAdmin(t *testing.T) *pgxpool.Pool {
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
	admin, err := pgxpool.New(ctx, pgSchemaDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "deployments", "migrations")
	downs, _ := filepath.Glob(filepath.Join(dir, "*.down.sql"))
	ups, _ := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if _, err := admin.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgTestSchema); err != nil {
		t.Fatal(err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(downs)))
	sort.Strings(ups)
	for _, f := range append(downs, ups...) {
		sql, _ := os.ReadFile(f)
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
	}
	role := pgx.Identifier{pgRLSRole()}.Sanitize()
	lit := "'" + strings.ReplaceAll(pgRLSRole(), "'", "''") + "'"
	for _, stmt := range []string{
		fmt.Sprintf(`DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF; END$$`, lit, role),
		`GRANT USAGE ON SCHEMA ` + pgTestSchema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON siem_exporters, siem_events, idempotency_keys TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return admin
}

// pgRouter is a fresh process: new pool, new stores.
func pgRouter(t *testing.T, az handler.AuthzChecker, withKey bool) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	cfg, _ := pgxpool.ParseConfig(pgSchemaDSN(os.Getenv("TEST_DATABASE_URL")))
	role := pgx.Identifier{pgRLSRole()}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var keys *credential.Keyring
	if withKey {
		keys, _ = credential.ParseKeys("k1", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), "")
	}
	h := handler.New(store.NewPgStore(pool, keys), az, zap.NewNop()).WithIdempotency(idempotency.NewPgStore(pool))
	return handler.NewRouter(h), pool
}

func pgCall(r http.Handler, method, path, tenant, principal, key string, body any) *httptest.ResponseRecorder {
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

const pgLiveToken = "dd-api-key-LIVE-0123456789abcdef"

func exporterBody() map[string]any {
	return map[string]any{"legal_entity_id": "le-1", "name": "dd", "platform": "DATADOG",
		"endpoint_url": "https://http-intake.example", "auth_token": pgLiveToken}
}

func idOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.ID == "" {
		t.Fatalf("no id in %d %s", w.Code, w.Body)
	}
	return v.ID
}

func TestPgRouter_PersistsAcrossRestartAndNeverReturnsToken(t *testing.T) {
	admin := pgAdmin(t)
	az := policyAuthz{grantAll("alice")}
	r1, _ := pgRouter(t, az, true)
	w := pgCall(r1, http.MethodPost, "/v1/siem/exporters", "t1", "alice", "", exporterBody())
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), pgLiveToken) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	id := idOf(t, w)
	// Service-to-service stream: tenant only, no principal, no envelope.
	ev := pgCall(r1, http.MethodPost, "/v1/siem/stream", "t1", "", "", map[string]any{
		"exporter_id": id, "source_service": "gateway-auth-svc", "event_type": "authentication.failed",
		"severity": "HIGH", "message": "bad password"})
	if ev.Code != http.StatusCreated {
		t.Fatalf("stream: %d %s", ev.Code, ev.Body)
	}

	r2, _ := pgRouter(t, az, true) // restart
	for _, path := range []string{"/v1/siem/exporters/" + id, "/v1/siem/exporters?legal_entity_id=le-1", "/v1/siem/events?exporter_id=" + id} {
		w := pgCall(r2, http.MethodGet, path, "t1", "alice", "", nil)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), pgLiveToken) || strings.Contains(w.Body.String(), "auth_token") {
			t.Fatalf("GET %s after restart: %d %s", path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), id) {
			t.Fatalf("GET %s after restart lost the record: %s", path, w.Body)
		}
	}
	var leaks int
	_ = admin.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM siem_exporters WHERE position($1 in row_to_json(siem_exporters)::text) > 0) +
		(SELECT count(*) FROM idempotency_keys WHERE position($1 in convert_from(coalesce(response_body, ''::bytea), 'UTF8')) > 0)`,
		pgLiveToken).Scan(&leaks)
	if leaks != 0 {
		t.Fatalf("plaintext token found in %d rows", leaks)
	}
}

func TestPgRouter_TokenWithoutSealingKeyIs503(t *testing.T) {
	admin := pgAdmin(t)
	r, _ := pgRouter(t, policyAuthz{grantAll("alice")}, false)
	w := pgCall(r, http.MethodPost, "/v1/siem/exporters", "t1", "alice", "", exporterBody())
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "credential_sealing_unavailable") {
		t.Fatalf("token without key: %d %s", w.Code, w.Body)
	}
	var n int
	_ = admin.QueryRow(context.Background(), `SELECT count(*) FROM siem_exporters`).Scan(&n)
	if n != 0 {
		t.Fatal("refused create persisted a row")
	}
}

func TestPgRouter_CrossTenantIs404(t *testing.T) {
	_ = pgAdmin(t)
	r, _ := pgRouter(t, policyAuthz{grantAll("alice")}, true)
	id := idOf(t, pgCall(r, http.MethodPost, "/v1/siem/exporters", "tenant-a", "alice", "", exporterBody()))
	if w := pgCall(r, http.MethodGet, "/v1/siem/exporters/"+id, "tenant-b", "alice", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant read: %d %s", w.Code, w.Body)
	}
	if w := pgCall(r, http.MethodGet, "/v1/siem/events?exporter_id="+id, "tenant-b", "alice", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant events: %d %s", w.Code, w.Body)
	}
	w := pgCall(r, http.MethodPost, "/v1/siem/stream", "tenant-b", "", "", map[string]any{
		"exporter_id": id, "source_service": "x", "event_type": "forged", "message": "m"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant event injection: %d %s", w.Code, w.Body)
	}
}

func TestPgRouter_AuthzAllowDenyAndMissingIdentity(t *testing.T) {
	admin := pgAdmin(t)
	grants := grantAll("alice")
	grants["bob|"+handler.SiemExporterRead] = true // bob may read exporters, not events
	r, _ := pgRouter(t, policyAuthz{grants}, true)
	id := idOf(t, pgCall(r, http.MethodPost, "/v1/siem/exporters", "t1", "alice", "", exporterBody()))

	if w := pgCall(r, http.MethodGet, "/v1/siem/exporters/"+id, "t1", "bob", "", nil); w.Code != http.StatusOK {
		t.Fatalf("granted read: %d", w.Code)
	}
	if w := pgCall(r, http.MethodGet, "/v1/siem/events?exporter_id="+id, "t1", "bob", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted event read: %d %s", w.Code, w.Body)
	}
	if w := pgCall(r, http.MethodPost, "/v1/siem/exporters", "t1", "mallory", "", exporterBody()); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted create: %d %s", w.Code, w.Body)
	}
	var n int
	_ = admin.QueryRow(context.Background(), `SELECT count(*) FROM siem_exporters`).Scan(&n)
	if n != 1 {
		t.Fatalf("denied create persisted: %d rows", n)
	}
	if w := pgCall(r, http.MethodGet, "/v1/siem/exporters/"+id, "", "alice", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant: %d", w.Code)
	}
	if w := pgCall(r, http.MethodGet, "/v1/siem/exporters/"+id, "t1", "", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: %d", w.Code)
	}
}

func TestPgRouter_DuplicateCreateIsReplayed(t *testing.T) {
	admin := pgAdmin(t)
	r, _ := pgRouter(t, policyAuthz{grantAll("alice")}, true)
	first := pgCall(r, http.MethodPost, "/v1/siem/exporters", "t1", "alice", "same-key", exporterBody())
	second := pgCall(r, http.MethodPost, "/v1/siem/exporters", "t1", "alice", "same-key", exporterBody())
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() ||
		second.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("replay: %d %s / %d %s", first.Code, first.Body, second.Code, second.Body)
	}
	var n int
	_ = admin.QueryRow(context.Background(), `SELECT count(*) FROM siem_exporters`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate request created %d exporters", n)
	}
	other := exporterBody()
	other["name"] = "different"
	if w := pgCall(r, http.MethodPost, "/v1/siem/exporters", "t1", "alice", "same-key", other); w.Code != http.StatusConflict {
		t.Fatalf("same key, different body: %d %s", w.Code, w.Body)
	}
}

func TestPgRouter_ReadyzFollowsTheDatabase(t *testing.T) {
	_ = pgAdmin(t)
	r, pool := pgRouter(t, policyAuthz{}, true)
	get := func(p string) int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		return w.Code
	}
	if get("/healthz") != 200 || get("/readyz") != 200 {
		t.Fatal("healthz/readyz must be 200 without identity headers while the database is up")
	}
	pool.Close()
	if c := get("/readyz"); c != http.StatusServiceUnavailable {
		t.Fatalf("readyz with database gone: %d", c)
	}
}

type downStore struct{ *store.MemoryStore }

func (downStore) Ping(context.Context) error { return errors.New("connection refused") }

func TestReadyz_UnavailableStoreIs503(t *testing.T) {
	r := handler.NewRouter(handler.New(downStore{store.NewMemoryStore()}, &stubAuthz{}, zap.NewNop()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: %d", w.Code)
	}
}
