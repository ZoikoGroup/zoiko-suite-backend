package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InFlightLease is how long an unanswered claim blocks a retry before it is
// presumed abandoned (crashed process) and may be taken over.
const InFlightLease = 2 * time.Minute

// PgStore keeps claims in the idempotency_keys table (see
// deployments/migrations/003_idempotency_keys.up.sql). Every statement runs in a transaction
// with app.tenant_id set, so the table's FORCEd row-level security applies.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore returns a store backed by pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Claim inserts the claim, or takes over an abandoned one with the same
// fingerprint, or reports the existing record.
func (s *PgStore) Claim(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*Record, error) {
	var existing *Record
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, endpoint, idempotency_key) DO UPDATE SET created_at = now()
			 WHERE idempotency_keys.response_status = 0
			   AND idempotency_keys.created_at < now() - make_interval(secs => $5)
			   AND idempotency_keys.request_fingerprint = EXCLUDED.request_fingerprint`,
			tenantID, endpoint, key, fingerprint, InFlightLease.Seconds())
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil // we hold the claim
		}

		var record Record
		err = tx.QueryRow(ctx, `
			SELECT request_fingerprint, response_status, response_body
			  FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
			tenantID, endpoint, key).Scan(&record.RequestFingerprint, &record.ResponseStatus, &record.ResponseBody)
		if err != nil {
			return fmt.Errorf("read idempotency key: %w", err)
		}
		if record.RequestFingerprint != fingerprint {
			return ErrFingerprintMismatch
		}
		existing = &record
		return nil
	})
	return existing, err
}

// Complete stores the terminal answer so later identical requests replay it.
func (s *PgStore) Complete(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET response_status = $1, response_body = $2
			 WHERE tenant_id = $3 AND endpoint = $4 AND idempotency_key = $5`,
			status, body, tenantID, endpoint, key)
		return err
	})
}

// Release drops an unanswered claim so a retry genuinely retries.
func (s *PgStore) Release(ctx context.Context, tenantID, endpoint, key string) error {
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3 AND response_status = 0`,
			tenantID, endpoint, key)
		return err
	})
}

// inTenantTx runs fn in a transaction scoped to tenantID for row-level security.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if s.pool == nil {
		return errors.New("idempotency store has no database pool")
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
