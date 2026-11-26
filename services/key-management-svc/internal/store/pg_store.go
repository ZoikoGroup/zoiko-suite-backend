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
	svcenvelope "zoiko.io/key-management-svc/internal/envelope"
	"zoiko.io/key-management-svc/internal/outbox"
)

// ErrInvalidTenant is returned for a tenant ID that is not a UUID. Every
// tenant the platform issues is one (see migration 000001); refusing here
// keeps a malformed header from ever reaching the ::uuid cast in the RLS
// policy.
var ErrInvalidTenant = errors.New("tenant_id must be a UUID")

// SourceService names this service on every event envelope.
const SourceService = "key-management-svc"

// Key lifecycle event types, written to the transactional outbox in the same
// transaction as the change. key.rotated and key.disabled keep the names the
// SIEM side-channel already uses.
const (
	EventKeyRegistered = "key.registered"
	EventKeyRotated    = "key.rotated"
	EventKeyDisabled   = "key.disabled"
)

// emitKeyEvent writes one key lifecycle event inside tx. The actor and
// correlation come from the request's envelope (installed by the middleware);
// a store call made outside a request carries neither, and the event is still
// written, unattributed, rather than the change going unrecorded.
func emitKeyEvent(ctx context.Context, tx pgx.Tx, eventType string, key *domain.CustomerKey) error {
	var actor, correlation string
	if env, ok := svcenvelope.FromContext(ctx); ok {
		actor, correlation = env.Actor(), env.CorrelationID
	}
	_, err := outbox.Insert(ctx, tx, SourceService, outbox.Event{
		AggregateType: "customer_key",
		AggregateID:   key.ID,
		EventType:     eventType,
		TenantID:      key.TenantID,
		LegalEntityID: key.LegalEntityID,
		ActorID:       actor,
		CorrelationID: correlation,
		Payload:       key,
	})
	return err
}

// PgStore is the durable Store and Vault: customer-key metadata and its
// version history (customer_keys, customer_key_versions) and the token vault
// (tokenized_values), in Postgres. It stores no key material.
//
// Every method runs in one transaction that first sets app.tenant_id for the
// RLS policies AND filters on tenant_id explicitly. Both, not either: RLS is
// bypassed entirely by a superuser connection, and the explicit filter is what
// still scopes the query if a deployment connects as one.
//
// Lifecycle changes (rotate, disable) take a row lock on the key with SELECT
// ... FOR UPDATE before changing it, so two concurrent rotations serialise:
// the second sees the first's committed version and advances from there. The
// partial unique index on customer_key_versions (one CURRENT per key) and the
// (key_id, version) primary key back that up in the database.
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

var (
	_ Store = (*PgStore)(nil)
	_ Vault = (*PgStore)(nil)
)

// Ping lets the store stand in for the pool in readiness checks.
func (s *PgStore) Ping(ctx context.Context) error {
	if s.pool == nil {
		return errors.New("no database pool")
	}
	return s.pool.Ping(ctx)
}

func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if _, err := uuid.Parse(tenantID); err != nil {
		return ErrInvalidTenant
	}
	if s.pool == nil {
		return errors.New("store has no database pool")
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
	k.CreatedAt = k.CreatedAt.UTC()
	k.UpdatedAt = k.UpdatedAt.UTC()
	if k.LastRotatedAt != nil {
		t := k.LastRotatedAt.UTC()
		k.LastRotatedAt = &t
	}
	return &k, nil
}

// CreateKey sets the server-owned fields — id, tenant, version 1, ENABLED,
// timestamps — and writes the key, its first CURRENT version row and its
// key.registered event in one transaction.
func (s *PgStore) CreateKey(ctx context.Context, tenantID string, key *domain.CustomerKey) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		ts := now()
		created, err := scanKey(tx.QueryRow(ctx, `
			INSERT INTO customer_keys (id, tenant_id, legal_entity_id, key_alias, key_model, key_provider,
				external_key_arn, key_version, state, rotation_count, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 0, $9, $9)
			RETURNING `+keyColumns,
			uuid.NewString(), tenantID, key.LegalEntityID, key.KeyAlias, key.KeyModel, key.KeyProvider,
			key.ExternalKeyARN, domain.StateEnabled, ts))
		if err != nil {
			return fmt.Errorf("insert customer key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_key_versions (tenant_id, key_id, version, status, activated_at)
			VALUES ($1, $2, 1, $3, $4)`,
			tenantID, created.ID, string(domain.VersionCurrent), ts); err != nil {
			return fmt.Errorf("insert key version: %w", err)
		}
		if err := emitKeyEvent(ctx, tx, EventKeyRegistered, created); err != nil {
			return err
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

// ListKeys: tenant and legal entity are both mandatory equality filters —
// legalEntityID is the authorization scope, so an empty one matches nothing
// rather than the whole tenant. (The handler always requires it.)
func (s *PgStore) ListKeys(ctx context.Context, tenantID, legalEntityID string) ([]domain.CustomerKey, error) {
	out := []domain.CustomerKey{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if legalEntityID == "" {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT `+keyColumns+` FROM customer_keys
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
		return nil, err
	}
	return out, nil
}

// RotateKey, in ONE transaction: lock the key row, retire its CURRENT version,
// insert version n+1 as CURRENT, advance key_version / rotation_count /
// last_rotated_at and write key.rotated. Any failure rolls all of it back, so
// a key can never be left with no current version, two current versions, or a
// key_version that disagrees with its version history.
//
// This records a rotation; it does not perform one. No key material is
// generated or re-wrapped here (tracker row 82).
func (s *PgStore) RotateKey(ctx context.Context, tenantID, id string) (*domain.CustomerKey, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrKeyNotFound
	}
	var out *domain.CustomerKey
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var current int
		if err := tx.QueryRow(ctx, `
			SELECT key_version FROM customer_keys
			 WHERE tenant_id = $1 AND id = $2
			 FOR UPDATE`, tenantID, id).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrKeyNotFound
			}
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
			SET key_version = key_version + 1, rotation_count = rotation_count + 1,
				last_rotated_at = $3, updated_at = $3
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+keyColumns, tenantID, id, ts))
		if err != nil {
			return err
		}
		out = k
		return emitKeyEvent(ctx, tx, EventKeyRotated, k)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DisableKey is a status transition, not a delete. It locks the key row;
// disabling an already disabled key is a no-op success and emits nothing.
func (s *PgStore) DisableKey(ctx context.Context, tenantID, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrKeyNotFound
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `
			SELECT state FROM customer_keys
			 WHERE tenant_id = $1 AND id = $2
			 FOR UPDATE`, tenantID, id).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrKeyNotFound
			}
			return err
		}
		if state == string(domain.StateDisabled) {
			return nil
		}
		k, err := scanKey(tx.QueryRow(ctx, `
			UPDATE customer_keys SET state = $3, updated_at = $4
			 WHERE tenant_id = $1 AND id = $2
			RETURNING `+keyColumns, tenantID, id, domain.StateDisabled, now()))
		if err != nil {
			return err
		}
		return emitKeyEvent(ctx, tx, EventKeyDisabled, k)
	})
}

// ListKeyVersions returns the key's version history, oldest first.
func (s *PgStore) ListKeyVersions(ctx context.Context, tenantID, id string) ([]domain.KeyVersion, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrKeyNotFound
	}
	var out []domain.KeyVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customer_keys WHERE tenant_id = $1 AND id = $2)`,
			tenantID, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrKeyNotFound
		}
		rows, err := tx.Query(ctx, `
			SELECT key_id::text, version, status, activated_at, retired_at
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
	if err != nil {
		return nil, err
	}
	return out, nil
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
