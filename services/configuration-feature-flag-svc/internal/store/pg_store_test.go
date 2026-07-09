package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	migPath := filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000001_initial_schema.up.sql")
	migSQL, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", migPath, err)
	}

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS feature_flags, config_entries CASCADE;`)
	if _, err := pool.Exec(ctx, string(migSQL)); err != nil {
		t.Fatalf("failed to execute migration: %v", err)
	}

	return pool
}

func strPtr(s string) *string { return &s }

// ── config_entries ───────────────────────────────────────────────────────────

func TestPgStore_UpsertConfigEntry_FirstWriteAndIdempotentSameValue(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	params := domain.UpsertConfigEntryParams{
		Key:                  "payroll.batch_size",
		Value:                []byte(`100`),
		Environment:          "staging",
		CreatedByPrincipalID: "admin-1",
	}

	// 1. First write.
	entry1, created, err := s.UpsertConfigEntry(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on first write: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on first write")
	}
	if entry1.EffectiveTo != nil {
		t.Errorf("expected effective_to nil on the currently-effective row")
	}

	// 2. Identical retry — same value, must be a no-op.
	entry2, created, err := s.UpsertConfigEntry(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical-value retry")
	}
	if entry2.ConfigID != entry1.ConfigID {
		t.Errorf("expected same config_id on no-op retry, got %s vs %s", entry2.ConfigID, entry1.ConfigID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM config_entries WHERE key = $1`, params.Key).Scan(&count); err != nil {
		t.Fatalf("failed to count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after an idempotent retry, got %d", count)
	}
}

func TestPgStore_UpsertConfigEntry_NewValueEndDatesOldRowNotDeletesIt(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	base := domain.UpsertConfigEntryParams{
		Key:                  "payroll.batch_size",
		Environment:          "staging",
		CreatedByPrincipalID: "admin-1",
	}

	v1Params := base
	v1Params.Value = []byte(`100`)
	v1, created, err := s.UpsertConfigEntry(ctx, v1Params)
	if err != nil {
		t.Fatalf("failed to write v1: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for v1")
	}

	v2Params := base
	v2Params.Value = []byte(`200`)
	v2, created, err := s.UpsertConfigEntry(ctx, v2Params)
	if err != nil {
		t.Fatalf("failed to write v2: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for a genuinely new value")
	}
	if v2.ConfigID == v1.ConfigID {
		t.Fatalf("expected a new row for a new value, got the same config_id")
	}

	// The whole point of this design: v1 must still exist, just end-dated.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM config_entries WHERE key = $1`, base.Key).Scan(&count); err != nil {
		t.Fatalf("failed to count rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows (old end-dated + new current), got %d", count)
	}

	var effectiveTo *string
	if err := pool.QueryRow(ctx, `SELECT effective_to::text FROM config_entries WHERE config_id = $1`, v1.ConfigID).Scan(&effectiveTo); err != nil {
		t.Fatalf("failed to fetch v1's effective_to: %v", err)
	}
	if effectiveTo == nil {
		t.Fatalf("expected v1's effective_to to be set after superseding, got NULL")
	}

	// GET must now return v2, not v1.
	current, err := s.FindCurrentConfigEntry(ctx, base.Key, base.Environment, nil)
	if err != nil {
		t.Fatalf("unexpected error finding current entry: %v", err)
	}
	if current.ConfigID != v2.ConfigID {
		t.Fatalf("expected current entry to be v2, got %s", current.ConfigID)
	}
}

func TestPgStore_FindCurrentConfigEntry_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.FindCurrentConfigEntry(ctx, "does.not.exist", "staging", nil)
	if !errors.Is(err, domain.ErrConfigEntryNotFound) {
		t.Fatalf("expected ErrConfigEntryNotFound, got %v", err)
	}
}

func TestPgStore_ConfigEntry_TenantScopeIsolation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := strPtr("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	global := domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging", CreatedByPrincipalID: "admin-1",
	}
	tenantSpecific := domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`999`), Environment: "staging", TenantID: tenantA, CreatedByPrincipalID: "admin-1",
	}

	if _, created, err := s.UpsertConfigEntry(ctx, global); err != nil || !created {
		t.Fatalf("failed to write global entry: created=%v err=%v", created, err)
	}
	if _, created, err := s.UpsertConfigEntry(ctx, tenantSpecific); err != nil || !created {
		t.Fatalf("failed to write tenant-specific entry: created=%v err=%v", created, err)
	}

	// Both must coexist as separate currently-effective rows — writing the
	// tenant-specific override must not have end-dated the global row.
	gotGlobal, err := s.FindCurrentConfigEntry(ctx, "payroll.batch_size", "staging", nil)
	if err != nil {
		t.Fatalf("unexpected error finding global entry: %v", err)
	}
	if string(gotGlobal.Value) != "100" {
		t.Errorf("expected global value 100, got %s", gotGlobal.Value)
	}

	gotTenant, err := s.FindCurrentConfigEntry(ctx, "payroll.batch_size", "staging", tenantA)
	if err != nil {
		t.Fatalf("unexpected error finding tenant-specific entry: %v", err)
	}
	if string(gotTenant.Value) != "999" {
		t.Errorf("expected tenant-specific value 999, got %s", gotTenant.Value)
	}
}

func TestPgStore_ListCurrentConfigEntries_FiltersByEnvironmentAndTenant(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := strPtr("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	writes := []domain.UpsertConfigEntryParams{
		{Key: "a", Value: []byte(`1`), Environment: "staging", CreatedByPrincipalID: "admin-1"},
		{Key: "b", Value: []byte(`2`), Environment: "production", CreatedByPrincipalID: "admin-1"},
		{Key: "c", Value: []byte(`3`), Environment: "staging", TenantID: tenantA, CreatedByPrincipalID: "admin-1"},
	}
	for _, p := range writes {
		if _, _, err := s.UpsertConfigEntry(ctx, p); err != nil {
			t.Fatalf("failed to seed %q: %v", p.Key, err)
		}
	}

	all, err := s.ListCurrentConfigEntries(ctx, store.ListFilter{})
	if err != nil {
		t.Fatalf("unexpected error listing all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries with no filter, got %d", len(all))
	}

	staging, err := s.ListCurrentConfigEntries(ctx, store.ListFilter{Environment: "staging"})
	if err != nil {
		t.Fatalf("unexpected error listing staging: %v", err)
	}
	if len(staging) != 2 {
		t.Fatalf("expected 2 entries for staging, got %d", len(staging))
	}

	tenantOnly, err := s.ListCurrentConfigEntries(ctx, store.ListFilter{Environment: "staging", TenantID: tenantA})
	if err != nil {
		t.Fatalf("unexpected error listing tenant-scoped: %v", err)
	}
	if len(tenantOnly) != 1 || tenantOnly[0].Key != "c" {
		t.Fatalf("expected exactly key=c for tenant-scoped staging, got %+v", tenantOnly)
	}
}

func TestPgStore_ConfigEntry_ErrorsWrapErrStoreUnavailable(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	if _, err := pool.Exec(ctx, `DROP TABLE config_entries CASCADE;`); err != nil {
		t.Fatalf("failed to drop table for test setup: %v", err)
	}

	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{Key: "k", Value: []byte(`1`), Environment: "staging", CreatedByPrincipalID: "admin-1"}); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("UpsertConfigEntry: expected ErrStoreUnavailable, got %v", err)
	}
	if _, err := s.FindCurrentConfigEntry(ctx, "k", "staging", nil); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("FindCurrentConfigEntry: expected ErrStoreUnavailable, got %v", err)
	}
	if _, err := s.ListCurrentConfigEntries(ctx, store.ListFilter{}); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("ListCurrentConfigEntries: expected ErrStoreUnavailable, got %v", err)
	}
}

// ── feature_flags ────────────────────────────────────────────────────────────

func TestPgStore_UpsertFeatureFlag_FirstWriteAndIdempotentSameValue(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	params := domain.UpsertFeatureFlagParams{
		Key: "new_ui", Enabled: true, Environment: "staging", RolloutPercentage: 50, CreatedByPrincipalID: "admin-1",
	}

	flag1, created, err := s.UpsertFeatureFlag(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on first write: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on first write")
	}

	flag2, created, err := s.UpsertFeatureFlag(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical-value retry")
	}
	if flag2.FlagID != flag1.FlagID {
		t.Errorf("expected same flag_id on no-op retry")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM feature_flags WHERE key = $1`, params.Key).Scan(&count); err != nil {
		t.Fatalf("failed to count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after an idempotent retry, got %d", count)
	}
}

func TestPgStore_UpsertFeatureFlag_RolloutPercentageChangeEndDatesOldRow(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	base := domain.UpsertFeatureFlagParams{Key: "new_ui", Enabled: true, Environment: "staging", CreatedByPrincipalID: "admin-1"}

	v1Params := base
	v1Params.RolloutPercentage = 10
	v1, _, err := s.UpsertFeatureFlag(ctx, v1Params)
	if err != nil {
		t.Fatalf("failed to write v1: %v", err)
	}

	v2Params := base
	v2Params.RolloutPercentage = 90
	v2, created, err := s.UpsertFeatureFlag(ctx, v2Params)
	if err != nil {
		t.Fatalf("failed to write v2: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for a rollout_percentage change")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM feature_flags WHERE key = $1`, base.Key).Scan(&count); err != nil {
		t.Fatalf("failed to count rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows (old end-dated + new current), got %d", count)
	}

	var effectiveTo *string
	if err := pool.QueryRow(ctx, `SELECT effective_to::text FROM feature_flags WHERE flag_id = $1`, v1.FlagID).Scan(&effectiveTo); err != nil {
		t.Fatalf("failed to fetch v1's effective_to: %v", err)
	}
	if effectiveTo == nil {
		t.Fatalf("expected v1's effective_to to be set after superseding, got NULL")
	}

	current, err := s.FindCurrentFeatureFlag(ctx, base.Key, base.Environment, nil)
	if err != nil {
		t.Fatalf("unexpected error finding current flag: %v", err)
	}
	if current.FlagID != v2.FlagID || current.RolloutPercentage != 90 {
		t.Fatalf("expected current flag to be v2 with rollout=90, got %+v", current)
	}
}

func TestPgStore_UpsertFeatureFlag_RolloutPercentageOutOfRangeRejectedByCheckConstraint(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	// The handler validates 0-100 before calling the store, but this
	// proves the DB-level CHECK constraint is itself a real safety net,
	// not just handler-side validation that could be bypassed by a future
	// caller of the store package.
	_, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "bad_flag", Enabled: true, Environment: "staging", RolloutPercentage: 150, CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Fatalf("expected the CHECK constraint violation to surface as ErrStoreUnavailable, got %v", err)
	}
}

func TestPgStore_FindCurrentFeatureFlag_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	_, err := s.FindCurrentFeatureFlag(ctx, "does_not_exist", "staging", nil)
	if !errors.Is(err, domain.ErrFeatureFlagNotFound) {
		t.Fatalf("expected ErrFeatureFlagNotFound, got %v", err)
	}
}

func TestPgStore_ListCurrentFeatureFlags_FiltersByEnvironmentAndTenant(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := strPtr("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	writes := []domain.UpsertFeatureFlagParams{
		{Key: "a", Enabled: true, Environment: "staging", CreatedByPrincipalID: "admin-1"},
		{Key: "b", Enabled: false, Environment: "production", CreatedByPrincipalID: "admin-1"},
		{Key: "c", Enabled: true, Environment: "staging", TenantID: tenantA, CreatedByPrincipalID: "admin-1"},
	}
	for _, p := range writes {
		if _, _, err := s.UpsertFeatureFlag(ctx, p); err != nil {
			t.Fatalf("failed to seed %q: %v", p.Key, err)
		}
	}

	staging, err := s.ListCurrentFeatureFlags(ctx, store.ListFilter{Environment: "staging"})
	if err != nil {
		t.Fatalf("unexpected error listing staging: %v", err)
	}
	if len(staging) != 2 {
		t.Fatalf("expected 2 flags for staging, got %d", len(staging))
	}

	tenantOnly, err := s.ListCurrentFeatureFlags(ctx, store.ListFilter{Environment: "staging", TenantID: tenantA})
	if err != nil {
		t.Fatalf("unexpected error listing tenant-scoped: %v", err)
	}
	if len(tenantOnly) != 1 || tenantOnly[0].Key != "c" {
		t.Fatalf("expected exactly key=c for tenant-scoped staging, got %+v", tenantOnly)
	}
}

func TestPgStore_FeatureFlag_ErrorsWrapErrStoreUnavailable(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	if _, err := pool.Exec(ctx, `DROP TABLE feature_flags CASCADE;`); err != nil {
		t.Fatalf("failed to drop table for test setup: %v", err)
	}

	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{Key: "k", Enabled: true, Environment: "staging", CreatedByPrincipalID: "admin-1"}); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("UpsertFeatureFlag: expected ErrStoreUnavailable, got %v", err)
	}
	if _, err := s.FindCurrentFeatureFlag(ctx, "k", "staging", nil); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("FindCurrentFeatureFlag: expected ErrStoreUnavailable, got %v", err)
	}
	if _, err := s.ListCurrentFeatureFlags(ctx, store.ListFilter{}); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("ListCurrentFeatureFlags: expected ErrStoreUnavailable, got %v", err)
	}
}
