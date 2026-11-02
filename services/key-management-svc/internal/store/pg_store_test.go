package store_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/store"
	"zoiko.io/key-management-svc/internal/testdb"
)

func newTenant() string { return "tenant-" + uuid.NewString() }

func newKey(le string) *domain.CustomerKey {
	return &domain.CustomerKey{
		LegalEntityID:  le,
		KeyAlias:       "alias-" + uuid.NewString()[:8],
		KeyModel:       domain.ModelBYOK,
		KeyProvider:    domain.ProviderAWSKMS,
		ExternalKeyARN: "arn:aws:kms:eu-west-2:111122223333:key/" + uuid.NewString(),
	}
}

func asTenant(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func versions(t *testing.T, s store.Store, tenant, id string) []domain.KeyVersion {
	t.Helper()
	vs, err := s.ListKeyVersions(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	return vs
}

func currentCount(vs []domain.KeyVersion) int {
	n := 0
	for _, v := range vs {
		if v.Status == domain.VersionCurrent {
			n++
		}
	}
	return n
}

// Key metadata and its lifecycle survive a restart: created, rotated and
// disabled through one pool, read back identically through a new pool/store.
func TestPgStore_PersistsAcrossRestart(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenant := newTenant()
	s := store.NewPgStore(app)

	k := newKey("le-1")
	if err := s.CreateKey(ctx, tenant, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.RotateKey(ctx, tenant, k.ID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := s.DisableKey(ctx, tenant, k.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	before, _ := s.GetKeyByID(ctx, tenant, k.ID)

	restarted, err := pgxpool.New(ctx, pools.AppDSN)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer restarted.Close()
	s2 := store.NewPgStore(restarted)
	after, err := s2.GetKeyByID(ctx, tenant, k.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("after restart:\n got  %+v\n want %+v", after, before)
	}
	if after.KeyVersion != 2 || after.RotationCount != 1 || after.State != domain.StateDisabled || after.LastRotatedAt == nil {
		t.Fatalf("lifecycle not persisted: %+v", after)
	}
	vs := versions(t, s2, tenant, k.ID)
	if len(vs) != 2 || vs[0].Status != domain.VersionRetired || vs[0].RetiredAt == nil || vs[1].Status != domain.VersionCurrent {
		t.Fatalf("version history after restart = %+v", vs)
	}
}

// Another tenant can neither read, list, rotate nor disable a key, and raw
// UPDATE/DELETE under its tenant scope touch 0 rows. The owner's key is
// unchanged afterwards.
func TestPgStore_CrossTenantReadUpdateDeleteDenied(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	s := store.NewPgStore(app)
	tenantA, tenantB := newTenant(), newTenant()

	k := newKey("le-1")
	if err := s.CreateKey(ctx, tenantA, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.GetKeyByID(ctx, tenantB, k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if list, err := s.ListKeys(ctx, tenantB, "le-1"); err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list = %d, %v", len(list), err)
	}
	if _, err := s.RotateKey(ctx, tenantB, k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant rotate: %v", err)
	}
	if err := s.DisableKey(ctx, tenantB, k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant disable: %v", err)
	}
	if _, err := s.ListKeyVersions(ctx, tenantB, k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant versions: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE customer_keys SET state = 'DISABLED' WHERE id = $1`,
		`DELETE FROM customer_key_versions WHERE key_id = $1`,
		`DELETE FROM customer_keys WHERE id = $1`,
	} {
		err := asTenant(t, app, tenantB, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt, k.ID)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%q as another tenant affected %d rows", stmt, tag.RowsAffected())
			}
			return err
		})
		if err != nil {
			t.Fatalf("%q: %v", stmt, err)
		}
	}
	got, err := s.GetKeyByID(ctx, tenantA, k.ID)
	if err != nil || got.State != domain.StateEnabled || got.KeyVersion != 1 {
		t.Fatalf("owner's key changed: %+v, %v", got, err)
	}
}

// The policies themselves, as a NOSUPERUSER NOBYPASSRLS role.
func TestPgStore_RLSBindsOrdinaryRole(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenant := newTenant()
	if err := store.NewPgStore(app).CreateKey(ctx, tenant, newKey("le-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, table := range []string{"customer_keys", "customer_key_versions"} {
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("%s with no tenant installed: app role saw %d rows, want 0", table, n)
		}
		if err := pools.Admin.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenant).Scan(&n); err != nil || n != 1 {
			t.Fatalf("negative control: owner sees %d rows in %s, %v; want 1", n, table, err)
		}
	}
	err := asTenant(t, app, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO customer_keys (id, tenant_id, legal_entity_id, key_alias, key_model, key_provider, external_key_arn, state)
			VALUES ($1, $2, 'le', 'a', 'BYOK', 'AWS_KMS', 'arn:x', 'ENABLED')`, uuid.NewString(), newTenant())
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("insert for another tenant: err = %v, want 42501", err)
	}
}

// Rotation is atomic: when inserting the next version fails, retiring the
// current one and advancing key_version are rolled back with it.
func TestPgStore_RotateRollsBackOnFailure(t *testing.T) {
	pools := testdb.Open(t)
	ctx := context.Background()
	s := store.NewPgStore(pools.Store())
	tenant := newTenant()
	k := newKey("le-1")
	if err := s.CreateKey(ctx, tenant, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Plant a conflicting version 2 (owner connection, bypassing the store)
	// so the rotation's INSERT hits the primary key.
	if _, err := pools.Admin.Exec(ctx, `
		INSERT INTO customer_key_versions (tenant_id, key_id, version, status, activated_at, retired_at)
		VALUES ($1, $2, 2, 'RETIRED', now(), now())`, tenant, k.ID); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if _, err := s.RotateKey(ctx, tenant, k.ID); err == nil {
		t.Fatal("rotate should fail on the planted version")
	}
	got, _ := s.GetKeyByID(ctx, tenant, k.ID)
	if got.KeyVersion != 1 || got.RotationCount != 0 || got.LastRotatedAt != nil {
		t.Fatalf("key advanced despite failed rotation: %+v", got)
	}
	vs := versions(t, s, tenant, k.ID)
	if vs[0].Version != 1 || vs[0].Status != domain.VersionCurrent || vs[0].RetiredAt != nil {
		t.Fatalf("version 1 was retired by a rolled-back rotation: %+v", vs)
	}
}

// Concurrent rotations serialise on the row lock: N rotations give version
// N+1, N+1 version rows, exactly one CURRENT.
func TestPgStore_ConcurrentRotationsSerialise(t *testing.T) {
	pools := testdb.Open(t)
	ctx := context.Background()
	s := store.NewPgStore(pools.Store())
	tenant := newTenant()
	k := newKey("le-1")
	if err := s.CreateKey(ctx, tenant, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.RotateKey(ctx, tenant, k.ID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent rotate: %v", err)
	}
	got, _ := s.GetKeyByID(ctx, tenant, k.ID)
	if got.KeyVersion != n+1 || got.RotationCount != n {
		t.Fatalf("after %d rotations: version %d, count %d", n, got.KeyVersion, got.RotationCount)
	}
	vs := versions(t, s, tenant, k.ID)
	if len(vs) != n+1 || currentCount(vs) != 1 || vs[n].Status != domain.VersionCurrent {
		t.Fatalf("version history = %+v", vs)
	}
}

// Disable is persisted and repeatable; unknown and malformed ids are not found.
func TestPgStore_DisableIsIdempotent(t *testing.T) {
	pools := testdb.Open(t)
	ctx := context.Background()
	s := store.NewPgStore(pools.Store())
	tenant := newTenant()
	k := newKey("le-1")
	if err := s.CreateKey(ctx, tenant, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.DisableKey(ctx, tenant, k.ID); err != nil {
			t.Fatalf("disable #%d: %v", i+1, err)
		}
	}
	got, _ := s.GetKeyByID(ctx, tenant, k.ID)
	if got.State != domain.StateDisabled {
		t.Fatalf("state = %s", got.State)
	}
	if err := s.DisableKey(ctx, tenant, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disable unknown: %v", err)
	}
	if err := s.DisableKey(ctx, tenant, "not-a-uuid"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disable malformed id: %v", err)
	}
}

// No column can hold key material: the schema has only references.
func TestSchema_HoldsNoKeyMaterial(t *testing.T) {
	pools := testdb.Open(t)
	rows, err := pools.Admin.Query(context.Background(), `
		SELECT table_name || '.' || column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name IN ('customer_keys', 'customer_key_versions')
		   AND (column_name ILIKE '%material%' OR column_name ILIKE '%secret%' OR column_name ILIKE '%private%'
		        OR column_name ILIKE '%plaintext%' OR data_type = 'bytea')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		t.Errorf("column %s looks like it could hold key material", c)
	}
}

// Every down migration reverses its up. Run in a private schema inside a
// transaction that is rolled back, so the tables other tests are using in
// public are never dropped, not even transiently: up, then down in reverse,
// then up again.
func TestMigrations_DownThenUp(t *testing.T) {
	pools := testdb.Open(t)
	ctx := context.Background()
	tx, err := pools.Admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE SCHEMA migration_roundtrip; SET LOCAL search_path TO migration_roundtrip`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	ups, downs := testdb.MigrationFiles(t, ".up.sql"), testdb.MigrationFiles(t, ".down.sql")
	if len(ups) != len(downs) {
		t.Fatalf("%d up migrations but %d down", len(ups), len(downs))
	}
	for _, f := range ups {
		execFile(t, tx, f)
	}
	for i := len(downs) - 1; i >= 0; i-- {
		execFile(t, tx, downs[i])
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'migration_roundtrip'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after down: %d tables remain, %v", n, err)
	}
	for _, f := range ups {
		execFile(t, tx, f)
	}
}

func execFile(t *testing.T, tx pgx.Tx, path string) {
	t.Helper()
	sql, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := tx.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("exec %s: %v", path, err)
	}
}
