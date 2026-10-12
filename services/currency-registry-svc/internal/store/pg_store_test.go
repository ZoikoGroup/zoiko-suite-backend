package store_test

// Real-Postgres suite. Same gating convention as the sibling services: skipped
// unless TEST_DATABASE_URL points at a Postgres this machine can use (and
// FAILED instead of skipped under CI / REQUIRE_DB_TESTS, since a skipped
// integration suite reports ok having verified nothing).
//
// STATUS: executed against PostgreSQL 16 (docker postgres:16-alpine) on 2026-10-07,
// all 4 tests passing, with the RLS test run as a NOSUPERUSER NOBYPASSRLS role that
// owns the schema (FORCE RLS applies to the owner). Run it the same way: a superuser
// DSN skips the RLS test. See RELEASE_CERTIFICATE.md.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/service"
	"zoiko.io/currency-registry-svc/internal/store"
)

func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set, but CI or REQUIRE_DB_TESTS demands these run. " +
				"A skipped integration suite reports ok having verified nothing.")
		}
		t.Skip("TEST_DATABASE_URL not set - skipping real-Postgres integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		DROP TABLE IF EXISTS currency_outbox, currency_idempotency, tenant_currency_support,
			currency_status_history, currency_minor_unit_versions, currency_imports, currencies CASCADE;
		DROP FUNCTION IF EXISTS currency_registry_codes_immutable() CASCADE;
		DROP FUNCTION IF EXISTS currency_registry_forbid_delete() CASCADE;
		DROP FUNCTION IF EXISTS currency_registry_forbid_mutation() CASCADE;`)
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations)
	sort.Strings(migrations)
	for _, p := range migrations {
		sqlText, err := os.ReadFile(p)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sqlText))
		require.NoError(t, err, "applying %s", filepath.Base(p))
	}
	return pool
}

func meta(actor, tenant, key, reason string) service.Meta {
	return service.Meta{Actor: actor, TenantID: tenant, CorrelationID: "corr-1", IdempotencyKey: key, RequestHash: "h-" + key, Reason: reason}
}

func rows() []domain.ImportRow {
	return []domain.ImportRow{{AlphaCode: "USD", NumericCode: "840", Name: "US Dollar", MinorUnit: json.Number("2")}}
}

func TestPg_ImportActivateOutboxAndIdempotency(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc := service.New(store.New(pool))

	in := service.ImportInput{Meta: meta("importer-1", "tenant-a", "k1", "update"), SourceName: "iso", SourceVersion: "1", Rows: rows()}
	in.ManifestHash = domain.ManifestHash(in.Rows)
	imp, replayed, err := svc.ImportCurrencyUpdate(ctx, in)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, domain.ImportApplied, imp.Status)

	// Replay under a new key returns the original; nothing duplicates.
	in2 := in
	in2.Meta = meta("importer-1", "tenant-a", "k2", "update")
	imp2, replayed, err := svc.ImportCurrencyUpdate(ctx, in2)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, imp.ImportID, imp2.ImportID)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM currencies`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM currency_minor_unit_versions`).Scan(&n))
	assert.Equal(t, 1, n)

	c, err := svc.GetCurrency(ctx, "USD", nil)
	require.NoError(t, err)
	_, _, err = svc.Transition(ctx, service.TransitionInput{Meta: meta("importer-1", "tenant-a", "k3", "go"), CurrencyID: c.CurrencyID, Command: service.CommandActivate, ExpectedVersion: c.Version})
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeSoDDenied, de.Code)

	got, _, err := svc.Transition(ctx, service.TransitionInput{Meta: meta("steward-2", "tenant-a", "k4", "go"), CurrencyID: c.CurrencyID, Command: service.CommandActivate, ExpectedVersion: c.Version})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSupported, got.Status)

	// Each state change has its event, written in the same transaction.
	// currency_outbox has FORCE ROW LEVEL SECURITY, so a bare count would see
	// nothing under a non-superuser role; count as the tenant that wrote them.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-a', true)`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM currency_outbox`).Scan(&n))
	require.NoError(t, tx.Rollback(ctx))
	assert.Equal(t, 2, n) // CurrencyUpdated + CurrencySupportChanged; the SoD refusal wrote nothing

	res, err := svc.Validate(ctx, "USD", domain.OperationPost, "tenant-a", false)
	require.NoError(t, err)
	assert.True(t, res.Supported)
}

func TestPg_AppendOnlyAndNoHardDelete(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc := service.New(store.New(pool))
	in := service.ImportInput{Meta: meta("importer-1", "tenant-a", "k1", "update"), SourceName: "iso", SourceVersion: "1", Rows: rows()}
	in.ManifestHash = domain.ManifestHash(in.Rows)
	_, _, err := svc.ImportCurrencyUpdate(ctx, in)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE currency_minor_unit_versions SET minor_unit = 3`)
	assert.Error(t, err, "minor-unit versions are append-only")
	_, err = pool.Exec(ctx, `DELETE FROM currency_minor_unit_versions`)
	assert.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM currencies`)
	assert.Error(t, err, "no hard delete of currencies")
	_, err = pool.Exec(ctx, `UPDATE currency_imports SET status = 'APPLIED'`)
	assert.Error(t, err, "import evidence is append-only")
	_, err = pool.Exec(ctx, `UPDATE currencies SET alpha_code = 'ZZZ'`)
	assert.Error(t, err, "codes are immutable")
	_, err = pool.Exec(ctx, `INSERT INTO currency_minor_unit_versions (currency_id, minor_unit, valid_from, source_version, evidence_ref)
		SELECT currency_id, 9, now() + interval '1 day', 'x', 'x' FROM currencies`)
	assert.Error(t, err, "minor_unit is constrained to 0..6")
}

func TestPg_TenantOverlayIsolationUnderRLS(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	var super bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	if super {
		t.Skip("TEST_DATABASE_URL connects as a superuser/BYPASSRLS role, which makes row-level security inert; use a plain role to run this test")
	}
	svc := service.New(store.New(pool))
	in := service.ImportInput{Meta: meta("importer-1", "tenant-a", "k1", "update"), SourceName: "iso", SourceVersion: "1", Rows: rows()}
	in.ManifestHash = domain.ManifestHash(in.Rows)
	_, _, err := svc.ImportCurrencyUpdate(ctx, in)
	require.NoError(t, err)
	c, err := svc.GetCurrency(ctx, "USD", nil)
	require.NoError(t, err)
	_, _, err = svc.Transition(ctx, service.TransitionInput{Meta: meta("steward-2", "tenant-a", "k2", "go"), CurrencyID: c.CurrencyID, Command: service.CommandActivate, ExpectedVersion: c.Version})
	require.NoError(t, err)

	_, _, err = svc.SetTenantSupport(ctx, service.TenantSupportInput{Meta: meta("ta", "tenant-a", "k3", "go"), TargetTenantID: "tenant-a", AlphaCode: "USD", Enable: true})
	require.NoError(t, err)

	a, err := svc.ListTenantSupport(ctx, "tenant-a")
	require.NoError(t, err)
	b, err := svc.ListTenantSupport(ctx, "tenant-b")
	require.NoError(t, err)
	assert.Len(t, a, 1)
	assert.Empty(t, b)

	// Even a query that FORGETS its tenant predicate sees nothing of A's rows from B's context.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-b', true)`)
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM tenant_currency_support`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM currency_outbox`).Scan(&n))
	assert.Equal(t, 0, n, "outbox rows of tenant-a are invisible to tenant-b")
}

func TestPg_ClaimOutboxPublishesOnce(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	st := store.New(pool)
	svc := service.New(st)
	in := service.ImportInput{Meta: meta("importer-1", "tenant-a", "k1", "update"), SourceName: "iso", SourceVersion: "1", Rows: rows()}
	in.ManifestHash = domain.ManifestHash(in.Rows)
	_, _, err := svc.ImportCurrencyUpdate(ctx, in)
	require.NoError(t, err)

	var got int
	require.NoError(t, st.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error { got = len(recs); return nil }))
	assert.Equal(t, 1, got)
	got = 0
	require.NoError(t, st.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error { got = len(recs); return nil }))
	assert.Equal(t, 0, got, "published rows are not claimed again")
	pending, _, err := st.OutboxDepth(ctx)
	require.NoError(t, err)
	assert.Zero(t, pending)
}
