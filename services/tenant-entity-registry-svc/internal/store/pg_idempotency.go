package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Idempotency-Key replay protection — migration 000010. Ported from
// identity-context-svc (internal/store/pg_idempotency.go), which fixed the same
// defect: the envelope demanded the key on every material write and nothing
// ever read it, so a retried command ran twice.

// IdempotencyRecord is a command's first response, kept so a retry can be
// answered with it rather than by doing the work again.
type IdempotencyRecord struct {
	RequestFingerprint string
	ResponseStatus     int
	ResponseBody       []byte
	CreatedAt          time.Time
}

// ErrIdempotencyFingerprintMismatch is returned when a key has been seen
// before against a DIFFERENT request.
var ErrIdempotencyFingerprintMismatch = errors.New("idempotency key reused with a different request")

// IdempotencyInFlightLease is how long an in-flight claim is honoured before it
// is treated as abandoned (its process died mid-command). It must comfortably
// exceed the longest command; the server's WriteTimeout is 15s.
const IdempotencyInFlightLease = 5 * time.Minute

// ClaimIdempotencyKey reserves (tenant, endpoint, key) for a request with this
// fingerprint.
//
// (nil, nil): claimed — execute the command. A non-nil record: the key was
// seen with the SAME fingerprint — replay that record (status 0 means still in
// flight). ErrIdempotencyFingerprintMismatch: seen with a different request.
//
// INSERT ... ON CONFLICT then a read of the winner, so two concurrent first
// attempts cannot both execute. An abandoned in-flight claim older than the
// lease is taken over by an identical request in the same statement.
func (s *PgStore) ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*IdempotencyRecord, error) {
	if tenantID == "" || endpoint == "" || key == "" {
		return nil, errors.New("ClaimIdempotencyKey: tenant_id, endpoint and idempotency_key are required")
	}

	var rec *IdempotencyRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys
				(tenant_id, endpoint, idempotency_key, request_fingerprint,
				 response_status, response_body)
			VALUES ($1,$2,$3,$4,0,'null'::jsonb)
			ON CONFLICT (tenant_id, endpoint, idempotency_key) DO UPDATE
			   SET created_at = NOW()
			 WHERE idempotency_keys.response_status = 0
			   AND idempotency_keys.created_at < NOW() - make_interval(secs => $5)
			   AND idempotency_keys.request_fingerprint = EXCLUDED.request_fingerprint`,
			tenantID, endpoint, key, fingerprint, IdempotencyInFlightLease.Seconds())
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}

		var existing IdempotencyRecord
		if err := tx.QueryRow(ctx, `
			SELECT request_fingerprint, response_status, response_body, created_at
			  FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
			tenantID, endpoint, key).Scan(
			&existing.RequestFingerprint, &existing.ResponseStatus,
			&existing.ResponseBody, &existing.CreatedAt); err != nil {
			return fmt.Errorf("read existing idempotency key: %w", err)
		}
		if existing.RequestFingerprint != fingerprint {
			return ErrIdempotencyFingerprintMismatch
		}
		rec = &existing
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// CompleteIdempotencyKey records the response a claimed command produced.
func (s *PgStore) CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	if len(body) == 0 || !json.Valid(body) {
		// jsonb accepts neither an empty body (204) nor chi's text/plain
		// refusals; 'null' round-trips as "no body".
		body = []byte("null")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE idempotency_keys
			   SET response_status = $1, response_body = $2
			 WHERE tenant_id = $3 AND endpoint = $4 AND idempotency_key = $5`,
			status, body, tenantID, endpoint, key); err != nil {
			return fmt.Errorf("complete idempotency key: %w", err)
		}
		return nil
	})
}

// ReleaseIdempotencyKey drops a claim whose command did not reach a terminal
// answer (5xx or panic), so a retry genuinely retries instead of being handed
// the transient failure forever.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3
			   AND response_status = 0`,
			tenantID, endpoint, key); err != nil {
			return fmt.Errorf("release idempotency key: %w", err)
		}
		return nil
	})
}

// PurgeIdempotencyKeysBefore drops recorded responses older than cutoff.
// Cross-tenant by construction, through 000010's named purge capability.
func (s *PgStore) PurgeIdempotencyKeysBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.idempotency_purge', 'true', true)"); err != nil {
		return 0, fmt.Errorf("set_config app.idempotency_purge: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge idempotency keys: %w", err)
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}
