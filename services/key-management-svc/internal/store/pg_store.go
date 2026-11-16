package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/key-management-svc/internal/domain"
)

// ErrInvalidTenant is returned for a tenant ID that is not a UUID. Every
// tenant the platform issues is one (see migration 000001); refusing here
// keeps a malformed header from ever reaching the ::uuid cast in the RLS
// policy.
var ErrInvalidTenant = errors.New("tenant_id must be a UUID")

// PgStore is the durable Store and Vault: customer-key metadata and the
// token vault in Postgres (migrations/000001).
//
// Every method runs in one transaction that first sets app.tenant_id for the
// RLS policies AND filters on tenant_id explicitly. Both, not either: RLS is
// bypassed entirely by a superuser connection, and the explicit filter is what
// still scopes the query if a deployment connects as one.
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

var (
	_ Store = (*PgStore)(nil)
	_ Vault = (*PgStore)(nil)
)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if _, err := uuid.Parse(tenantID); err != nil {
		return ErrInvalidTenant
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const keyColumns = `id, tenant_id::text, legal_entity_id, key_alias, key_model, key_provider, external_key_arn,
	key_version, state, rotation_count, last_rotated_at, created_at, updated_at`

func scanKey(row pgx.Row) (*domain.CustomerKey, error) {
	var k domain.CustomerKey
	if err := row.Scan(&k.ID, &k.TenantID, &k.LegalEntityID, &k.KeyAlias, &k.KeyModel, &k.KeyProvider,
		&k.ExternalKeyARN, &k.KeyVersion, &k.State, &k.RotationCount, &k.LastRotatedAt, &k.CreatedAt, &k.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	return &k, nil
}

// CreateKey sets the same server-owned fields MemoryStore always set — id,
// tenant, version 1, ENABLED, timestamps — and writes the row.
func (s *PgStore) CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		created, err := scanKey(tx.QueryRow(ctx, `
			INSERT INTO customer_keys (id, tenant_id, legal_entity_id, key_alias, key_model, key_provider,
				external_key_arn, key_version, state, rotation_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 0)
			RETURNING `+keyColumns,
			uuid.NewString(), tenantID, key.LegalEntityID, key.KeyAlias, key.KeyModel, key.KeyProvider,
			key.ExternalKeyARN, domain.StateEnabled))
		if err != nil {
			return fmt.Errorf("insert customer key: %w", err)
		}
		*key = *created
		return nil
	})
}

func (s *PgStore) GetKeyByID(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrKeyNotFound // no such key can exist
	}
	var out *domain.CustomerKey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		k, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyColumns+` FROM customer_keys WHERE tenant_id = $1 AND id = $2`, tenantID, id))
		out = k
		return err
	})
	return out, err
}

// ListKeys matches MemoryStore: an empty legalEntityID lists the whole
// tenant (the handler never passes one; it requires the parameter).
func (s *PgStore) ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error) {
	var out []domain.CustomerKey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+keyColumns+` FROM customer_keys
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2)
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
	return out, err
}

// RotateKey advances the version in one statement, so two concurrent
// rotations cannot both read version n and both write n+1.
func (s *PgStore) RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrKeyNotFound
	}
	var out *domain.CustomerKey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		k, err := scanKey(tx.QueryRow(ctx, `
			UPDATE customer_keys
			SET key_version = key_version + 1, rotation_count = rotation_count + 1,
				last_rotated_at = NOW(), updated_at = NOW()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+keyColumns, tenantID, id))
		out = k
		return err
	})
	return out, err
}

// DisableKey is a status transition, not a delete.
func (s *PgStore) DisableKey(ctx context.Context, tenantID, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrKeyNotFound
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE customer_keys SET state = $3, updated_at = NOW()
			WHERE tenant_id = $1 AND id = $2`, tenantID, id, domain.StateDisabled)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrKeyNotFound
		}
		return nil
	})
}

const tokenColumns = `token_id::text, tenant_id::text, legal_entity_id, encrypted_value, classification,
	created_by_principal_id, idempotency_key, created_at`

func scanToken(row pgx.Row) (*domain.TokenizedValue, error) {
	var t domain.TokenizedValue
	if err := row.Scan(&t.TokenID, &t.TenantID, &t.LegalEntityID, &t.EncryptedValue, &t.Classification,
		&t.CreatedByPrincipalID, &t.IdempotencyKey, &t.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTokenNotFound
		}
		return nil, err
	}
	return &t, nil
}

// CreateToken inserts the row, or — when this tenant already used the
// idempotency key — returns the existing row untouched. ON CONFLICT DO
// NOTHING makes the claim atomic: of two concurrent requests with one key,
// exactly one inserts, and the other waits for it and then reads its row.
func (s *PgStore) CreateToken(ctx context.Context, tenantID string, v *domain.TokenizedValue) (*domain.TokenizedValue, bool, error) {
	var existing *domain.TokenizedValue
	created := false
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO tokenized_values (token_id, tenant_id, legal_entity_id, encrypted_value, classification,
				created_by_principal_id, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
			RETURNING created_at`,
			v.TokenID, tenantID, v.LegalEntityID, v.EncryptedValue, v.Classification,
			v.CreatedByPrincipalID, v.IdempotencyKey).Scan(&v.CreatedAt)
		if err == nil {
			v.TenantID, created = tenantID, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("insert tokenized value: %w", err)
		}
		existing, err = scanToken(tx.QueryRow(ctx, `SELECT `+tokenColumns+` FROM tokenized_values
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, v.IdempotencyKey))
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return existing, created, nil
}

func (s *PgStore) GetToken(ctx context.Context, tenantID, tokenID string) (*domain.TokenizedValue, error) {
	if _, err := uuid.Parse(tokenID); err != nil {
		return nil, domain.ErrTokenNotFound // no such token can exist
	}
	var out *domain.TokenizedValue
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		t, err := scanToken(tx.QueryRow(ctx, `SELECT `+tokenColumns+` FROM tokenized_values
			WHERE tenant_id = $1 AND token_id = $2`, tenantID, tokenID))
		out = t
		return err
	})
	return out, err
}
