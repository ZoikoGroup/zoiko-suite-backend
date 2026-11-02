package store_test

import (
	"context"
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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/mtls-management-svc/internal/domain"
	"zoiko.io/mtls-management-svc/internal/idempotency"
	"zoiko.io/mtls-management-svc/internal/store"
)

// testSchema isolates this package's tables in their own schema, so packages
// that `go test ./...` runs in parallel against one database never reset each
// other's tables mid-test.
const testSchema = "mtls_store_test"

func schemaDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + testSchema
}

// ── harness ────────────────────────────────────────────────────────────────
//
// TEST_DATABASE_URL: a disposable database (name must contain "test" or
// "b810"), connected as a role that may run DDL; the suite drops and
// re-applies this service's migrations. Skips when unset.
//
// TEST_RLS_ROLE: an existing NOSUPERUSER NOBYPASSRLS role. The store under
// test runs every connection as it (SET ROLE), so row-level security really
// applies — as a superuser it would be inert. Default "app_rls_probe" is
// created NOLOGIN when missing.

func migrationsDir(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deployments", "migrations")
}

func adminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL: %v", err)
	}
	db := strings.ToLower(strings.TrimPrefix(u.Path, "/"))
	if !strings.Contains(db, "test") && !strings.Contains(db, "b810") {
		t.Fatalf("refusing to run: database %q is not recognisably disposable and this suite drops tables", db)
	}
	pool, err := pgxpool.New(context.Background(), schemaDSN(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func rlsRole() string {
	if r := os.Getenv("TEST_RLS_ROLE"); r != "" {
		return r
	}
	return "app_rls_probe"
}

// resetSchema runs every down migration (newest first) then every up
// migration, so both directions are exercised on each run.
func resetSchema(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	dir := migrationsDir(t)
	downs, _ := filepath.Glob(filepath.Join(dir, "*.down.sql"))
	ups, _ := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if len(ups) == 0 || len(ups) != len(downs) {
		t.Fatalf("migrations: %d up, %d down", len(ups), len(downs))
	}
	if _, err := admin.Exec(context.Background(), `CREATE SCHEMA IF NOT EXISTS `+testSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(downs)))
	sort.Strings(ups)
	for _, f := range append(downs, ups...) {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	role := pgx.Identifier{rlsRole()}.Sanitize()
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DO $$BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
			CREATE ROLE %s NOLOGIN NOSUPERUSER NOBYPASSRLS;
		END IF; END$$`, quoteLiteral(rlsRole()), role)); err != nil {
		t.Fatalf("ensure rls role: %v", err)
	}
	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA ` + testSchema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON mtls_certificates, mtls_communication_policies, idempotency_keys TO ` + role,
		`GRANT ` + role + ` TO CURRENT_USER`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil && !strings.Contains(err.Error(), "already") {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// appPool is a fresh pool whose every connection runs as the RLS role. A new
// appPool is a new process as far as the store can tell.
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

func setup(t *testing.T) (*pgxpool.Pool, *store.PgStore) {
	admin := adminPool(t)
	resetSchema(t, admin)
	return admin, store.NewPgStore(appPool(t))
}

func newCert(le, svc string) *domain.MtlsCertificate {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &domain.MtlsCertificate{
		LegalEntityID: le, ServiceName: svc, CommonName: svc, Issuer: "ZoikoSuite Internal CA",
		SerialNumber: strings.ReplaceAll(uuid.NewString(), "-", ""), Fingerprint: "fp-" + uuid.NewString(),
		CertificatePEM: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		ValidFrom:      now, ValidTo: now.Add(90 * 24 * time.Hour), RotationDays: 90,
		Status: domain.CertStatusActive,
	}
}

func rotate(t *testing.T, s store.Store, tenant, id string) (*domain.MtlsCertificate, string, error) {
	serial := strings.ReplaceAll(uuid.NewString(), "-", "")
	now := time.Now().UTC()
	c, err := s.ReplaceCertMaterial(context.Background(), tenant, id, serial, "fp-"+serial, "pem-"+serial, now, now.Add(time.Hour))
	return c, serial, err
}

// ── tests ──────────────────────────────────────────────────────────────────

func TestPg_StoreRunsAsOrdinaryRole(t *testing.T) {
	_, _ = setup(t)
	var super, bypass bool
	var user string
	err := appPool(t).QueryRow(context.Background(),
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&user, &super, &bypass)
	if err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("store connections run as %s (superuser=%v bypassrls=%v): RLS would be inert", user, super, bypass)
	}
}

func TestPg_RecordsSurviveANewPool(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	c := newCert("le-1", "svc-a")
	if err := s.CreateCert(ctx, "t1", c); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rotate(t, s, "t1", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeCert(ctx, "t1", c.ID); err != nil {
		t.Fatal(err)
	}
	pol := &domain.CommunicationPolicy{PolicyName: "p", SourceService: "a", TargetService: "b", Action: domain.PolicyAllow, RequiresMtls: true}
	if err := s.CreatePolicy(ctx, "t1", pol); err != nil {
		t.Fatal(err)
	}

	restarted := store.NewPgStore(appPool(t)) // simulated process restart
	got, err := restarted.GetCertByID(ctx, "t1", c.ID)
	if err != nil {
		t.Fatalf("certificate lost across restart: %v", err)
	}
	if got.Status != domain.CertStatusRevoked || got.RevokedAt == nil {
		t.Fatalf("revocation lost across restart: %+v", got)
	}
	if len(got.PriorSerialNumbers) != 1 || got.PriorSerialNumbers[0] != c.SerialNumber {
		t.Fatalf("rotation history lost: prior=%v want [%s]", got.PriorSerialNumbers, c.SerialNumber)
	}
	pols, err := restarted.ListPolicies(ctx, "t1")
	if err != nil || len(pols) != 1 || pols[0].ID != pol.ID || !pols[0].RequiresMtls {
		t.Fatalf("policy lost across restart: %v %+v", err, pols)
	}
}

func TestPg_CrossTenantReadUpdateRevokeDenied(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	c := newCert("le-1", "svc-a")
	if err := s.CreateCert(ctx, "tenant-a", c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCertByID(ctx, "tenant-b", c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	if _, _, err := rotate(t, s, "tenant-b", c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant rotate: %v", err)
	}
	if _, err := s.RevokeCert(ctx, "tenant-b", c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant revoke: %v", err)
	}
	if l, err := s.ListCerts(ctx, "tenant-b", "", ""); err != nil || len(l) != 0 {
		t.Fatalf("cross-tenant list: %v %d", err, len(l))
	}
	got, err := s.GetCertByID(ctx, "tenant-a", c.ID)
	if err != nil || got.Status != domain.CertStatusActive || got.SerialNumber != c.SerialNumber {
		t.Fatalf("owner's record was changed by another tenant: %v %+v", err, got)
	}
}

// The policy itself, not the store's predicates: raw SQL as the ordinary role.
func TestPg_RLSPolicyBindsRawSQL(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	c := newCert("le-1", "svc-a")
	if err := s.CreateCert(ctx, "tenant-a", c); err != nil {
		t.Fatal(err)
	}
	app := appPool(t)
	inTx := func(tenant string, fn func(pgx.Tx) error) error {
		tx, err := app.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if tenant != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				return err
			}
		}
		return fn(tx)
	}
	for _, tenant := range []string{"", "tenant-b"} {
		_ = inTx(tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM mtls_certificates`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("tenant %q sees %d foreign rows without a predicate", tenant, n)
			}
			tag, err := tx.Exec(ctx, `UPDATE mtls_certificates SET status = 'REVOKED', revoked_at = now()`)
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("tenant %q updated %d foreign rows", tenant, tag.RowsAffected())
			}
			tag, err = tx.Exec(ctx, `DELETE FROM mtls_certificates`)
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("tenant %q deleted %d foreign rows", tenant, tag.RowsAffected())
			}
			return nil
		})
	}
	err := inTx("tenant-b", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO mtls_communication_policies (id, tenant_id, policy_name) VALUES ($1, 'tenant-a', 'smuggled')`, uuid.NewString())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("WITH CHECK must refuse writing a row for another tenant, got %v", err)
	}
}

func TestPg_RevokedIsTerminal(t *testing.T) {
	admin, s := setup(t)
	ctx := context.Background()
	c := newCert("le-1", "svc-a")
	if err := s.CreateCert(ctx, "t1", c); err != nil {
		t.Fatal(err)
	}
	first, err := s.RevokeCert(ctx, "t1", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RevokeCert(ctx, "t1", c.ID)
	if err != nil || !again.RevokedAt.Equal(*first.RevokedAt) {
		t.Fatalf("second revoke must be idempotent and keep revoked_at: %v %v %v", err, first.RevokedAt, again.RevokedAt)
	}
	if _, _, err := rotate(t, s, "t1", c.ID); !errors.Is(err, store.ErrCertRevoked) {
		t.Fatalf("rotating a revoked record: %v", err)
	}
	// The database refuses it too, even for the table owner bypassing the store.
	tx, _ := admin.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 't1', true)`)
	if _, err := tx.Exec(ctx, `UPDATE mtls_certificates SET status = 'ACTIVE', revoked_at = NULL WHERE id = $1`, c.ID); err == nil {
		t.Fatal("un-revoking via SQL must be refused by the trigger")
	}
}

// A failed statement inside the tenant transaction leaves nothing behind.
func TestPg_FailedWriteRollsBack(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	a := newCert("le-1", "svc-a")
	if err := s.CreateCert(ctx, "t1", a); err != nil {
		t.Fatal(err)
	}
	dup := newCert("le-1", "svc-b")
	dup.SerialNumber = a.SerialNumber // unique index violation
	if err := s.CreateCert(ctx, "t1", dup); err == nil {
		t.Fatal("duplicate serial must be refused")
	}
	if _, err := s.GetCertByID(ctx, "t1", dup.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed insert left a row: %v", err)
	}
	if l, _ := s.ListCerts(ctx, "t1", "", ""); len(l) != 1 {
		t.Fatalf("want 1 record after rollback, got %d", len(l))
	}
	// A rotation whose new serial collides is rolled back whole: the record
	// keeps its old serial and gains no prior serial.
	b := newCert("le-1", "svc-c")
	if err := s.CreateCert(ctx, "t1", b); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.ReplaceCertMaterial(ctx, "t1", b.ID, a.SerialNumber, "fp", "pem", now, now.Add(time.Hour)); err == nil {
		t.Fatal("rotation onto an existing serial must fail")
	}
	got, _ := s.GetCertByID(ctx, "t1", b.ID)
	if got.SerialNumber != b.SerialNumber || len(got.PriorSerialNumbers) != 0 {
		t.Fatalf("partial rotation persisted: %+v", got)
	}
}

// Concurrent rotations serialise on the row lock: every superseded serial is
// kept, none is lost to a read-modify-write race.
func TestPg_ConcurrentRotationsLoseNoSerial(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	c := newCert("le-1", "svc-a")
	if err := s.CreateCert(ctx, "t1", c); err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	serials := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, serial, err := rotate(t, s, "t1", c.ID)
			if err != nil {
				t.Error(err)
				return
			}
			serials <- serial
		}()
	}
	wg.Wait()
	close(serials)
	got, err := s.GetCertByID(ctx, "t1", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]bool{}
	for _, sn := range got.AllSerialNumbers() {
		all[sn] = true
	}
	if !all[c.SerialNumber] {
		t.Error("original serial lost")
	}
	for sn := range serials {
		if !all[sn] {
			t.Errorf("rotated serial %s lost", sn)
		}
	}
	if len(got.AllSerialNumbers()) != n+1 {
		t.Fatalf("want %d serials, got %d", n+1, len(got.AllSerialNumbers()))
	}
}

// A rotation racing a revocation either lands before it (and its serial is in
// the revoked record) or is refused — never after it.
func TestPg_RotateRacingRevokeNeverEscapesRevocation(t *testing.T) {
	_, s := setup(t)
	ctx := context.Background()
	for round := 0; round < 5; round++ {
		c := newCert("le-1", fmt.Sprintf("svc-%d", round))
		if err := s.CreateCert(ctx, "t1", c); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var issued []string
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, serial, err := rotate(t, s, "t1", c.ID)
				if err == nil {
					mu.Lock()
					issued = append(issued, serial)
					mu.Unlock()
				} else if !errors.Is(err, store.ErrCertRevoked) {
					t.Error(err)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.RevokeCert(ctx, "t1", c.ID); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		final, err := s.GetCertByID(ctx, "t1", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != domain.CertStatusRevoked {
			t.Fatalf("round %d: status %s", round, final.Status)
		}
		covered := map[string]bool{}
		for _, sn := range final.AllSerialNumbers() {
			covered[sn] = true
		}
		for _, sn := range issued {
			if !covered[sn] {
				t.Fatalf("round %d: serial %s issued by a rotation is not covered by the revoked record", round, sn)
			}
		}
	}
}

func TestPg_IdempotencyStoreIsTenantScopedAndWithholdsSecrets(t *testing.T) {
	admin, _ := setup(t)
	ctx := context.Background()
	idem := idempotency.NewPgStore(appPool(t))
	if rec, err := idem.Claim(ctx, "t1", "POST /v1/mtls/certificates", "k1", "fp1"); err != nil || rec != nil {
		t.Fatalf("first claim: %v %v", rec, err)
	}
	if err := idem.Complete(ctx, "t1", "POST /v1/mtls/certificates", "k1", 201, []byte(`{"private_key_pem":"SECRET"}`), true); err != nil {
		t.Fatal(err)
	}
	rec, err := idem.Claim(ctx, "t1", "POST /v1/mtls/certificates", "k1", "fp1")
	if err != nil || rec == nil || !rec.SecretWithheld || rec.ResponseStatus != 201 || len(rec.ResponseBody) != 0 {
		t.Fatalf("replay record: %+v %v", rec, err)
	}
	// Same key in another tenant is a different claim.
	if rec, err := idem.Claim(ctx, "t2", "POST /v1/mtls/certificates", "k1", "other"); err != nil || rec != nil {
		t.Fatalf("other tenant's claim must be independent: %v %v", rec, err)
	}
	var leaked int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE response_body::text LIKE '%SECRET%' OR convert_from(coalesce(response_body,''::bytea),'UTF8') LIKE '%SECRET%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("%d idempotency rows hold the secret body", leaked)
	}
}
