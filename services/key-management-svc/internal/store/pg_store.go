package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/key-management-svc/internal/domain"
)

// PgStore persists key metadata in customer_keys and its version history in
// customer_key_versions (migration 000001). It stores no key material.
//
// Every statement runs inside a transaction that first installs
// app.tenant_id, so both tables' FORCEd row-level security policies bind the
// query as well as the explicit tenant_id predicate each statement carries.
//
// Lifecycle changes (rotate, disable) take a row lock on the key with SELECT
// ... FOR UPDATE before changing it, so two concurrent rotations serialise:
// the second sees the first's committed version and advances from there,
// instead of both computing the same "next" version. The partial unique index
// on customer_key_versions (one CURRENT per key) and the (key_id, version)
// primary key back that up in the database.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore returns a store backed by pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Ping lets the store stand in for the pool in readiness checks.
func (s *PgStore) Ping(ctx context.Context) error {
	if s.pool == nil {
		return errors.New("no database pool")
	}
	return s.pool.Ping(ctx)
}

func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// CreateKey inserts the key at version 1, ENABLED, together with its first
// CURRENT version row, in one transaction. The id, tenant, version, state and
// timestamps are assigned here, never taken from the caller.
func (s *PgStore) CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error {
	if tenantID == "" {
		return errors.New("tenant is required")
	}
	id := uuid.New().String()
	ts := now()
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_keys
			    (id, tenant_id, legal_entity_id, key_alias, key_model, key_provider,
			     external_key_arn, key_version, state, rotation_count, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 0, $9, $9)`,
			id, tenantID, key.LegalEntityID, key.KeyAlias, string(key.KeyModel), string(key.KeyProvider),
			key.ExternalKeyARN, string(domain.StateEnabled), ts); err != nil {
			return fmt.Errorf("insert key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_key_versions (tenant_id, key_id, version, status, activated_at)
			VALUES ($1, $2, 1, $3, $4)`,
			tenantID, id, string(domain.VersionCurrent), ts); err != nil {
			return fmt.Errorf("insert key version: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	key.ID = id
	key.TenantID = tenantID
	key.KeyVersion = 1
	key.RotationCount = 0
	key.LastRotatedAt = nil
	key.State = domain.StateEnabled
	key.CreatedAt = ts
	key.UpdatedAt = ts
	return nil
}

const selectKey = `
	SELECT id, tenant_id, legal_entity_id, key_alias, key_model, key_provider,
	       external_key_arn, key_version, state, rotation_count, last_rotated_at,
	       created_at, updated_at
	  FROM customer_keys`

// GetKeyByID returns ErrNotFound for an unknown id, a malformed id, and
// another tenant's id alike.
func (s *PgStore) GetKeyByID(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	if tenantID == "" || !validID(id) {
		return nil, ErrNotFound
	}
	var out *domain.CustomerKey
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		k, err := scanKey(tx.QueryRow(ctx, selectKey+` WHERE tenant_id = $1 AND id = $2`, tenantID, id))
		out = k
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get key: %w", err)
	}
	return out, nil
}

// ListKeys: tenant and legal entity are both mandatory equality filters.
func (s *PgStore) ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error) {
	out := []domain.CustomerKey{}
	if tenantID == "" || legalEntityID == "" {
		return out, nil
	}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, selectKey+`
			 WHERE tenant_id = $1 AND legal_entity_id = $2
			 ORDER BY created_at, id`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			k, err := scanKey(rows)
			if err != nil {
				return err
			}
			out = append(out, *k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	return out, nil
}

// RotateKey, in ONE transaction: lock the key row, retire its CURRENT version,
// insert version n+1 as CURRENT, and advance key_version / rotation_count /
// last_rotated_at. Any failure rolls all of it back, so a key can never be
// left with no current version, two current versions, or a key_version that
// disagrees with its version history.
//
// This records a rotation; it does not perform one. No key material is
// generated or re-wrapped here (tracker row 82).
func (s *PgStore) RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	if tenantID == "" || !validID(id) {
		return nil, ErrNotFound
	}
	var out *domain.CustomerKey
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var current int
		if err := tx.QueryRow(ctx, `
			SELECT key_version FROM customer_keys
			 WHERE tenant_id = $1 AND id = $2
			 FOR UPDATE`, tenantID, id).Scan(&current); err != nil {
			return err
		}
		ts := now()
		tag, err := tx.Exec(ctx, `
			UPDATE customer_key_versions SET status = $1, retired_at = $2
			 WHERE tenant_id = $3 AND key_id = $4 AND version = $5 AND status = $6`,
			string(domain.VersionRetired), ts, tenantID, id, current, string(domain.VersionCurrent))
		if err != nil {
			return fmt.Errorf("retire version %d: %w", current, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("retire version %d: key has no current version at that number", current)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_key_versions (tenant_id, key_id, version, status, activated_at)
			VALUES ($1, $2, $3, $4, $5)`,
			tenantID, id, current+1, string(domain.VersionCurrent), ts); err != nil {
			return fmt.Errorf("insert version %d: %w", current+1, err)
		}
		k, err := scanKey(tx.QueryRow(ctx, `
			UPDATE customer_keys
			   SET key_version = key_version + 1,
			       rotation_count = rotation_count + 1,
			       last_rotated_at = $3,
			       updated_at = $3
			 WHERE tenant_id = $1 AND id = $2
			RETURNING id, tenant_id, legal_entity_id, key_alias, key_model, key_provider,
			          external_key_arn, key_version, state, rotation_count, last_rotated_at,
			          created_at, updated_at`, tenantID, id, ts))
		out = k
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rotate key: %w", err)
	}
	return out, nil
}

// DisableKey locks the key row and sets it DISABLED. Disabling an already
// disabled key is a no-op success, matching the in-memory behaviour.
func (s *PgStore) DisableKey(ctx context.Context, tenantID, id string) error {
	if tenantID == "" || !validID(id) {
		return ErrNotFound
	}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `
			SELECT state FROM customer_keys
			 WHERE tenant_id = $1 AND id = $2
			 FOR UPDATE`, tenantID, id).Scan(&state); err != nil {
			return err
		}
		if state == string(domain.StateDisabled) {
			return nil
		}
		_, err := tx.Exec(ctx, `
			UPDATE customer_keys SET state = $3, updated_at = $4
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, string(domain.StateDisabled), now())
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("disable key: %w", err)
	}
	return nil
}

// ListKeyVersions returns the key's version history, oldest first.
func (s *PgStore) ListKeyVersions(ctx context.Context, tenantID, id string) ([]domain.KeyVersion, error) {
	if tenantID == "" || !validID(id) {
		return nil, ErrNotFound
	}
	var out []domain.KeyVersion
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customer_keys WHERE tenant_id = $1 AND id = $2)`,
			tenantID, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return pgx.ErrNoRows
		}
		rows, err := tx.Query(ctx, `
			SELECT key_id, version, status, activated_at, retired_at
			  FROM customer_key_versions
			 WHERE tenant_id = $1 AND key_id = $2
			 ORDER BY version`, tenantID, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v domain.KeyVersion
			var status string
			if err := rows.Scan(&v.KeyID, &v.Version, &status, &v.ActivatedAt, &v.RetiredAt); err != nil {
				return err
			}
			v.Status = domain.KeyVersionStatus(status)
			v.ActivatedAt = v.ActivatedAt.UTC()
			if v.RetiredAt != nil {
				t := v.RetiredAt.UTC()
				v.RetiredAt = &t
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("list key versions: %w", err)
	}
	return out, nil
}

func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

func scanKey(row pgx.Row) (*domain.CustomerKey, error) {
	var (
		k                      domain.CustomerKey
		model, provider, state string
	)
	if err := row.Scan(&k.ID, &k.TenantID, &k.LegalEntityID, &k.KeyAlias, &model, &provider,
		&k.ExternalKeyARN, &k.KeyVersion, &state, &k.RotationCount, &k.LastRotatedAt,
		&k.CreatedAt, &k.UpdatedAt); err != nil {
		return nil, err
	}
	k.KeyModel = domain.KeyModel(model)
	k.KeyProvider = domain.KeyProvider(provider)
	k.State = domain.KeyState(state)
	k.CreatedAt = k.CreatedAt.UTC()
	k.UpdatedAt = k.UpdatedAt.UTC()
	if k.LastRotatedAt != nil {
		t := k.LastRotatedAt.UTC()
		k.LastRotatedAt = &t
	}
	return &k, nil
}

// inTenantTx runs fn in a transaction scoped to tenantID for row-level
// security. set_config(..., true) is transaction-local, so the scope cannot
// leak to the next borrower of the pooled connection.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if s.pool == nil {
		return errors.New("store has no database pool")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
