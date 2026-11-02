package store_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/siem-integration-svc/internal/credential"
	"zoiko.io/siem-integration-svc/internal/domain"
	"zoiko.io/siem-integration-svc/internal/store"
)

// TEST_DATABASE_URL: disposable database (name contains "test" or "b810"),
// as a role that may run DDL; tables live in their own schema (testSchema) and
// are dropped and re-migrated per test. Skips when unset.
// TEST_RLS_ROLE: an existing NOSUPERUSER NOBYPASSRLS role every store
// connection runs as (SET ROLE), so row-level security really applies.
// Default "app_rls_probe" is created NOLOGIN when missing.

const testSchema = "siem_store_test"

func schemaDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + testSchema
}

func rlsRole() string {
	if r := os.Getenv("TEST_RLS_ROLE"); r != "" {
		return r
	}
	return "app_rls_probe"
}

func adminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if db := strings.ToLower(strings.TrimPrefix(u.Path, "/")); !strings.Contains(db, "test") && !strings.Contains(db, "b810") {
		t.Fatalf("refusing to run: database %q is not recognisably disposable", db)
	}
	pool, err := pgxpool.New(context.Background(), schemaDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func resetSchema(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "deployments", "migrations")
	downs, _ := filepath.Glob(filepath.Join(dir, "*.down.sql"))
	ups, _ := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if len(ups) == 0 || len(ups) != len(downs) {
		t.Fatalf("migrations: %d up, %d down", len(ups), len(downs))
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+testSchema); err != nil {
		t.Fatal(err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(downs)))
	sort.Strings(ups)
	for _, f := range append(downs, ups...) {
		sql, _ := os.ReadFile(f)
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	role := pgx.Identifier{rlsRole()}.Sanitize()
	lit := "'" + strings.ReplaceAll(rlsRole(), "'", "''") + "'"
	for _, stmt := range []string{
		fmt.Sprintf(`DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN CREATE ROLE %s NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF; END$$`, lit, role),
		`GRANT USAGE ON SCHEMA ` + testSchema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON siem_exporters, siem_events, idempotency_keys TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(schemaDSN(os.Getenv("TEST_DATABASE_URL")))
	if err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{rlsRole()}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testKeys(t *testing.T) *credential.Keyring {
	kr, err := credential.ParseKeys("k1", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func setup(t *testing.T) (*pgxpool.Pool, *store.PgStore) {
	admin := adminPool(t)
	resetSchema(t, admin)
	return admin, store.NewPgStore(appPool(t), testKeys(t))
}

const liveToken = "splunk-hec-LIVE-TOKEN-0123456789"

func newExporter(le string) *domain.SIEMExporter {
	return &domain.SIEMExporter{LegalEntityID: le, Name: "e-" + uuid.NewString()[:8], Platform: domain.PlatformSplunk,
		EndpointURL: "https://siem.example/hec", AuthToken: liveToken}
}

func event(exporterID string) *domain.SIEMEvent {
	return &domain.SIEMEvent{ExporterID: exporterID, SourceSvc: "gateway-auth-svc", EventType: "authentication.failed",
		Severity: domain.SeverityHigh, Message: "bad password"}
}

func TestPg_StoreRunsAsOrdinaryRole(t *testing.T) {
	_, _ = setup(t)
	var super, bypass bool
	if err := appPool(t).QueryRow(context.Background(),
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("store runs as superuser=%v bypassrls=%v: RLS would be inert", super, bypass)
	}
}

func TestPg_RecordsSurviveANewPool(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	if err := s.CreateExporter(ctx, "t1", exp); err != nil {
		t.Fatal(err)
	}
	if exp.AuthToken != "" {
		t.Fatal("the plaintext token must be cleared from the struct after sealing")
	}
	if err := s.StreamEvent(ctx, "t1", event(exp.ID)); err != nil {
		t.Fatal(err)
	}
	restarted := store.NewPgStore(appPool(t), testKeys(t))
	got, err := restarted.GetExporterByID(ctx, "t1", exp.ID)
	if err != nil || got.EventsSent != 1 || got.LastStreamed == nil || got.Status != domain.ExporterActive {
		t.Fatalf("exporter lost across restart: %v %+v", err, got)
	}
	evts, err := restarted.ListEvents(ctx, "t1", exp.ID)
	if err != nil || len(evts) != 1 || evts[0].EventType != "authentication.failed" {
		t.Fatalf("events lost across restart: %v %+v", err, evts)
	}
}

func TestPg_TokenIsSealedAtRestAndNeverRead(t *testing.T) {
	admin, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	if err := s.CreateExporter(ctx, "t1", exp); err != nil {
		t.Fatal(err)
	}
	var ct, kid string
	if err := admin.QueryRow(ctx, `SELECT auth_token_ciphertext, auth_token_key_id FROM siem_exporters WHERE id = $1`, exp.ID).Scan(&ct, &kid); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, liveToken) || kid != "k1" {
		t.Fatalf("token stored in plaintext or without key id: kid=%q", kid)
	}
	var plaintextRows int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM siem_exporters WHERE position($1 in row_to_json(siem_exporters)::text) > 0`, liveToken).Scan(&plaintextRows)
	if plaintextRows != 0 {
		t.Fatal("plaintext token found in siem_exporters")
	}
	plain, err := testKeys(t).Open(ct, kid, credential.ExporterAAD("t1", exp.ID))
	if err != nil || string(plain) != liveToken {
		t.Fatalf("sealed token must open with the key and its row AAD: %v", err)
	}
	got, _ := s.GetExporterByID(ctx, "t1", exp.ID)
	list, _ := s.ListExporters(ctx, "t1", "")
	if got.AuthToken != "" || list[0].AuthToken != "" {
		t.Fatal("reads must never carry the token")
	}
}

func TestPg_NoKeyRefusesTokenButAllowsTokenless(t *testing.T) {
	admin, _ := setup(t)
	s := store.NewPgStore(appPool(t), nil)
	ctx := context.Background()
	if err := s.CreateExporter(ctx, "t1", newExporter("le-1")); !errors.Is(err, store.ErrCredentialSealingUnavailable) {
		t.Fatalf("token without a sealing key: %v", err)
	}
	var n int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM siem_exporters`).Scan(&n)
	if n != 0 {
		t.Fatal("refused create persisted a row")
	}
	e := newExporter("le-1")
	e.AuthToken = ""
	if err := s.CreateExporter(ctx, "t1", e); err != nil {
		t.Fatalf("tokenless exporter: %v", err)
	}
}

func TestPg_CrossTenantDenied(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	if err := s.CreateExporter(ctx, "tenant-a", exp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExporterByID(ctx, "tenant-b", exp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	if err := s.StreamEvent(ctx, "tenant-b", event(exp.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant event injection: %v", err)
	}
	if l, _ := s.ListExporters(ctx, "tenant-b", ""); len(l) != 0 {
		t.Fatal("cross-tenant list")
	}
	if l, _ := s.ListEvents(ctx, "tenant-b", exp.ID); len(l) != 0 {
		t.Fatal("cross-tenant events")
	}
	got, _ := s.GetExporterByID(ctx, "tenant-a", exp.ID)
	if got.EventsSent != 0 {
		t.Fatal("another tenant advanced this exporter's counter")
	}
}

func TestPg_RLSPolicyBindsRawSQL(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	if err := s.CreateExporter(ctx, "tenant-a", exp); err != nil {
		t.Fatal(err)
	}
	_ = s.StreamEvent(ctx, "tenant-a", event(exp.ID))
	app := appPool(t)
	for _, tenant := range []string{"", "tenant-b"} {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tenant != "" {
			_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
		}
		var ne, nv int
		_ = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM siem_exporters), (SELECT count(*) FROM siem_events)`).Scan(&ne, &nv)
		tag, err := tx.Exec(ctx, `UPDATE siem_exporters SET endpoint_url = 'https://attacker.example'`)
		if err != nil {
			t.Fatal(err)
		}
		if ne != 0 || nv != 0 || tag.RowsAffected() != 0 {
			t.Errorf("tenant %q: sees %d exporters, %d events; redirected %d exporters", tenant, ne, nv, tag.RowsAffected())
		}
		_ = tx.Rollback(ctx)
	}
	tx, _ := app.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-b', true)`)
	_, err := tx.Exec(ctx, `INSERT INTO siem_exporters (id, tenant_id, legal_entity_id, name, platform, endpoint_url, status)
		VALUES ($1, 'tenant-a', 'le', 'x', 'SPLUNK', 'u', 'ACTIVE')`, uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("WITH CHECK must refuse a row for another tenant: %v", err)
	}
}

// Doc 05 §13.4: security events are append-only, even for the owner.
func TestPg_EventsAreAppendOnly(t *testing.T) {
	admin, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	_ = s.CreateExporter(ctx, "t1", exp)
	_ = s.StreamEvent(ctx, "t1", event(exp.ID))
	for _, stmt := range []string{`UPDATE siem_events SET message = 'nothing happened'`, `DELETE FROM siem_events`} {
		if _, err := admin.Exec(ctx, stmt); err == nil {
			t.Errorf("%s must be refused", stmt)
		}
	}
	if l, _ := s.ListEvents(ctx, "t1", exp.ID); len(l) != 1 || l[0].Message != "bad password" {
		t.Fatalf("event altered: %+v", l)
	}
}

// Counter and event are one transaction: a failed event insert leaves the
// counter where it was.
func TestPg_FailedStreamRollsBackCounter(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	_ = s.CreateExporter(ctx, "t1", exp)
	bad := event(exp.ID)
	bad.Severity = "BOGUS" // violates the CHECK after the counter UPDATE ran
	if err := s.StreamEvent(ctx, "t1", bad); err == nil {
		t.Fatal("invalid severity must fail")
	}
	got, _ := s.GetExporterByID(ctx, "t1", exp.ID)
	if got.EventsSent != 0 || got.LastStreamed != nil {
		t.Fatalf("counter advanced without an event: %+v", got)
	}
}

func TestPg_ConcurrentStreamsLoseNoCount(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	exp := newExporter("le-1")
	_ = s.CreateExporter(ctx, "t1", exp)
	const n = 25
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.StreamEvent(ctx, "t1", event(exp.ID)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := s.GetExporterByID(ctx, "t1", exp.ID)
	evts, _ := s.ListEvents(ctx, "t1", exp.ID)
	if got.EventsSent != n || len(evts) != n {
		t.Fatalf("events_sent=%d events=%d, want %d", got.EventsSent, len(evts), n)
	}
}
