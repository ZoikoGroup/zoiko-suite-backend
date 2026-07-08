// Package store provides the PostgreSQL implementation of the
// configuration and feature-flag read/write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
//
// Idempotency design (context.md §7.3): POST /v1/config and POST
// /v1/flags are upserts, not plain inserts. Each write is wrapped in a
// transaction that SELECTs the currently-effective row for the target
// scope FOR UPDATE (serializing concurrent writers for that exact scope),
// then either no-ops (value unchanged — safe to repeat), or end-dates the
// current row and inserts a new one. The partial unique index on each
// table (deployments/migrations/000001_initial_schema.up.sql) is the
// backstop that makes this safe even if the transactional logic here had
// a bug — Postgres itself will refuse a second concurrently-effective row
// for the same scope.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// nilScopeUUID is the sentinel used in COALESCE() to make the
// one-currently-effective-row-per-scope constraint and lookup queries
// work across a nullable tenant_id column — mirrors policy-svc's handling
// of nullable tenant_id/legal_entity_id.
const nilScopeUUID = "00000000-0000-0000-0000-000000000000"

// ListFilter narrows a List query by optional environment/tenant_id.
// Both empty/nil means "no filter on that dimension".
type ListFilter struct {
	Environment string
	TenantID    *string
}

// Store is the persistence interface for config entries and feature flags.
type Store interface {
	// UpsertConfigEntry writes a new config entry version for the given
	// scope, or no-ops if the value is unchanged from what's currently
	// effective. Returns created=true only when a real transition
	// happened (first write for this scope, or a genuinely new value).
	UpsertConfigEntry(ctx context.Context, params domain.UpsertConfigEntryParams) (entry *domain.ConfigEntry, created bool, err error)

	// FindCurrentConfigEntry returns the row currently effective
	// (effective_to IS NULL) for the exact (key, environment, tenant_id)
	// scope. No fallback from a tenant-specific miss to a global default.
	FindCurrentConfigEntry(ctx context.Context, key, environment string, tenantID *string) (*domain.ConfigEntry, error)

	// ListCurrentConfigEntries returns every currently-effective entry,
	// optionally filtered by environment and/or tenant_id.
	ListCurrentConfigEntries(ctx context.Context, filter ListFilter) ([]*domain.ConfigEntry, error)

	// UpsertFeatureFlag is UpsertConfigEntry's counterpart for
	// feature_flags, comparing (enabled, rollout_percentage) instead of a
	// JSON value for equality.
	UpsertFeatureFlag(ctx context.Context, params domain.UpsertFeatureFlagParams) (flag *domain.FeatureFlag, created bool, err error)

	// FindCurrentFeatureFlag is FindCurrentConfigEntry's counterpart.
	FindCurrentFeatureFlag(ctx context.Context, key, environment string, tenantID *string) (*domain.FeatureFlag, error)

	// ListCurrentFeatureFlags is ListCurrentConfigEntries's counterpart.
	ListCurrentFeatureFlags(ctx context.Context, filter ListFilter) ([]*domain.FeatureFlag, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// ── config_entries ───────────────────────────────────────────────────────────

const configColumns = `
	config_id,
	key,
	value,
	environment,
	tenant_id,
	effective_from,
	effective_to,
	created_by_principal_id,
	created_at`

func scanConfigEntry(row pgx.Row) (*domain.ConfigEntry, error) {
	c := &domain.ConfigEntry{}
	err := row.Scan(
		&c.ConfigID,
		&c.Key,
		&c.Value,
		&c.Environment,
		&c.TenantID,
		&c.EffectiveFrom,
		&c.EffectiveTo,
		&c.CreatedByPrincipalID,
		&c.CreatedAt,
	)
	return c, err
}

// UpsertConfigEntry implements the upsert-with-value-equality design in
// context.md §7.3.
func (s *PgStore) UpsertConfigEntry(ctx context.Context, params domain.UpsertConfigEntryParams) (*domain.ConfigEntry, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg UpsertConfigEntry: begin tx failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	const findCurrentQuery = `
		SELECT ` + configColumns + `
		FROM config_entries
		WHERE key = $1
		  AND environment = $2
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND effective_to IS NULL
		FOR UPDATE;`

	current, err := scanConfigEntry(tx.QueryRow(ctx, findCurrentQuery, params.Key, params.Environment, params.TenantID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No current row for this scope — first write.
		entry, insertErr := insertConfigEntry(ctx, tx, params)
		if insertErr != nil {
			return nil, false, insertErr
		}
		if err := tx.Commit(ctx); err != nil {
			s.log.Error("pg UpsertConfigEntry: commit failed", zap.Error(err))
			return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return entry, true, nil

	case err != nil:
		s.log.Error("pg UpsertConfigEntry: find current failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// A current row exists. Same value → idempotent no-op, safe to
	// repeat. jsonEqual is semantic (unmarshal + reflect.DeepEqual), not
	// bytes.Equal — Postgres re-serialises JSONB with its own whitespace
	// conventions, so a byte comparison would spuriously fail on every
	// legitimate retry (same bug class documented in policy-svc's
	// CreatePolicyVersion).
	if jsonEqual(current.Value, params.Value) {
		return current, false, nil
	}

	// Different value → end-date the current row and insert the new one,
	// same transaction.
	const endDateQuery = `UPDATE config_entries SET effective_to = NOW() WHERE config_id = $1;`
	if _, err := tx.Exec(ctx, endDateQuery, current.ConfigID); err != nil {
		s.log.Error("pg UpsertConfigEntry: end-date failed", zap.String("config_id", current.ConfigID), zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	entry, err := insertConfigEntry(ctx, tx, params)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg UpsertConfigEntry: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return entry, true, nil
}

// insertConfigEntry inserts a brand-new currently-effective row. Shared by
// both the "no current row" and "value changed" branches of
// UpsertConfigEntry.
func insertConfigEntry(ctx context.Context, tx pgx.Tx, params domain.UpsertConfigEntryParams) (*domain.ConfigEntry, error) {
	const insertQuery = `
		INSERT INTO config_entries (key, value, environment, tenant_id, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + configColumns + `;`

	entry, err := scanConfigEntry(tx.QueryRow(ctx, insertQuery,
		params.Key, params.Value, params.Environment, params.TenantID, params.CreatedByPrincipalID,
	))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return entry, nil
}

// jsonEqual compares two JSON payloads structurally rather than
// byte-for-byte. See package doc comment / policy-svc precedent.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// FindCurrentConfigEntry looks up the currently-effective row for an
// exact (key, environment, tenant_id) scope. No fallback to a global
// default on a tenant-specific miss — see context.md §7.2.
func (s *PgStore) FindCurrentConfigEntry(ctx context.Context, key, environment string, tenantID *string) (*domain.ConfigEntry, error) {
	const query = `
		SELECT ` + configColumns + `
		FROM config_entries
		WHERE key = $1
		  AND environment = $2
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND effective_to IS NULL;`

	entry, err := scanConfigEntry(s.pool.QueryRow(ctx, query, key, environment, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrConfigEntryNotFound
		}
		s.log.Error("pg FindCurrentConfigEntry failed", zap.String("key", key), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return entry, nil
}

// ListCurrentConfigEntries returns every currently-effective config entry,
// optionally filtered by environment and/or tenant_id. An absent filter
// dimension means "no filter" (not "global only") — e.g. omitting
// tenant_id returns entries across all tenants, not just global ones.
func (s *PgStore) ListCurrentConfigEntries(ctx context.Context, filter ListFilter) ([]*domain.ConfigEntry, error) {
	args := []any{}
	conditions := []string{"effective_to IS NULL"}
	argIdx := 1

	if filter.Environment != "" {
		conditions = append(conditions, fmt.Sprintf("environment = $%d", argIdx))
		args = append(args, filter.Environment)
		argIdx++
	}
	if filter.TenantID != nil {
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", argIdx))
		args = append(args, *filter.TenantID)
		argIdx++
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM config_entries
		WHERE %s
		ORDER BY key, environment;`,
		configColumns, strings.Join(conditions, " AND "),
	)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg ListCurrentConfigEntries failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.ConfigEntry
	for rows.Next() {
		c, scanErr := scanConfigEntry(rows)
		if scanErr != nil {
			s.log.Error("pg ListCurrentConfigEntries scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, c)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg ListCurrentConfigEntries rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// ── feature_flags ────────────────────────────────────────────────────────────

const flagColumns = `
	flag_id,
	key,
	enabled,
	environment,
	tenant_id,
	rollout_percentage,
	effective_from,
	effective_to,
	created_by_principal_id,
	created_at`

func scanFeatureFlag(row pgx.Row) (*domain.FeatureFlag, error) {
	f := &domain.FeatureFlag{}
	err := row.Scan(
		&f.FlagID,
		&f.Key,
		&f.Enabled,
		&f.Environment,
		&f.TenantID,
		&f.RolloutPercentage,
		&f.EffectiveFrom,
		&f.EffectiveTo,
		&f.CreatedByPrincipalID,
		&f.CreatedAt,
	)
	return f, err
}

// UpsertFeatureFlag is UpsertConfigEntry's counterpart for feature_flags —
// same transactional shape, comparing (enabled, rollout_percentage) for
// equality instead of a JSON value.
func (s *PgStore) UpsertFeatureFlag(ctx context.Context, params domain.UpsertFeatureFlagParams) (*domain.FeatureFlag, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg UpsertFeatureFlag: begin tx failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const findCurrentQuery = `
		SELECT ` + flagColumns + `
		FROM feature_flags
		WHERE key = $1
		  AND environment = $2
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND effective_to IS NULL
		FOR UPDATE;`

	current, err := scanFeatureFlag(tx.QueryRow(ctx, findCurrentQuery, params.Key, params.Environment, params.TenantID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		flag, insertErr := insertFeatureFlag(ctx, tx, params)
		if insertErr != nil {
			return nil, false, insertErr
		}
		if err := tx.Commit(ctx); err != nil {
			s.log.Error("pg UpsertFeatureFlag: commit failed", zap.Error(err))
			return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return flag, true, nil

	case err != nil:
		s.log.Error("pg UpsertFeatureFlag: find current failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if current.Enabled == params.Enabled && current.RolloutPercentage == params.RolloutPercentage {
		return current, false, nil
	}

	const endDateQuery = `UPDATE feature_flags SET effective_to = NOW() WHERE flag_id = $1;`
	if _, err := tx.Exec(ctx, endDateQuery, current.FlagID); err != nil {
		s.log.Error("pg UpsertFeatureFlag: end-date failed", zap.String("flag_id", current.FlagID), zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	flag, err := insertFeatureFlag(ctx, tx, params)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg UpsertFeatureFlag: commit failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return flag, true, nil
}

func insertFeatureFlag(ctx context.Context, tx pgx.Tx, params domain.UpsertFeatureFlagParams) (*domain.FeatureFlag, error) {
	const insertQuery = `
		INSERT INTO feature_flags (key, enabled, environment, tenant_id, rollout_percentage, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + flagColumns + `;`

	flag, err := scanFeatureFlag(tx.QueryRow(ctx, insertQuery,
		params.Key, params.Enabled, params.Environment, params.TenantID, params.RolloutPercentage, params.CreatedByPrincipalID,
	))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return flag, nil
}

// FindCurrentFeatureFlag is FindCurrentConfigEntry's counterpart.
func (s *PgStore) FindCurrentFeatureFlag(ctx context.Context, key, environment string, tenantID *string) (*domain.FeatureFlag, error) {
	const query = `
		SELECT ` + flagColumns + `
		FROM feature_flags
		WHERE key = $1
		  AND environment = $2
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND effective_to IS NULL;`

	flag, err := scanFeatureFlag(s.pool.QueryRow(ctx, query, key, environment, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFeatureFlagNotFound
		}
		s.log.Error("pg FindCurrentFeatureFlag failed", zap.String("key", key), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return flag, nil
}

// ListCurrentFeatureFlags is ListCurrentConfigEntries's counterpart.
func (s *PgStore) ListCurrentFeatureFlags(ctx context.Context, filter ListFilter) ([]*domain.FeatureFlag, error) {
	args := []any{}
	conditions := []string{"effective_to IS NULL"}
	argIdx := 1

	if filter.Environment != "" {
		conditions = append(conditions, fmt.Sprintf("environment = $%d", argIdx))
		args = append(args, filter.Environment)
		argIdx++
	}
	if filter.TenantID != nil {
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", argIdx))
		args = append(args, *filter.TenantID)
		argIdx++
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM feature_flags
		WHERE %s
		ORDER BY key, environment;`,
		flagColumns, strings.Join(conditions, " AND "),
	)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg ListCurrentFeatureFlags failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.FeatureFlag
	for rows.Next() {
		f, scanErr := scanFeatureFlag(rows)
		if scanErr != nil {
			s.log.Error("pg ListCurrentFeatureFlags scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, f)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg ListCurrentFeatureFlags rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// ─── compile-time interface check ──────────────────────────────────────────

var _ Store = (*PgStore)(nil)
