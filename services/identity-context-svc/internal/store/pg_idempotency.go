package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// IdempotencyRecord is a command's first response, kept so a retry can be
// answered with it rather than by doing the work twice.
type IdempotencyRecord struct {
	RequestFingerprint string
	ResponseStatus     int
	ResponseBody       []byte
	CreatedAt          time.Time
}

// ErrIdempotencyFingerprintMismatch is returned when a key has been seen
// before against a DIFFERENT request.
//
// This is the case ErrCodeIdempotencyMismatch was declared for in
// domain/gov01.go and which, before migration 000008, no code path in this
// service could detect: the envelope middleware demanded an Idempotency-Key
// on every material write and nothing ever read one.
var ErrIdempotencyFingerprintMismatch = errors.New("idempotency key reused with a different request")

// ClaimIdempotencyKey attempts to reserve (tenant, endpoint, key) for a
// request with this fingerprint.
//
// Returns (nil, nil) when the claim succeeded and the caller should execute
// the command. Returns a non-nil record when the key has been seen before with
// the SAME fingerprint — a genuine retry, which the caller must answer by
// replaying that record rather than acting again. Returns
// ErrIdempotencyFingerprintMismatch when the key has been seen with a
// different fingerprint.
//
// The claim is an INSERT ... ON CONFLICT DO NOTHING followed by a read of the
// conflicting row, so two concurrent first attempts cannot both execute: one
// inserts, the other sees the winner's row. The loser's row carries status 0
// until the winner finishes, which the caller reads as "in flight".
func (s *PgStore) ClaimIdempotencyKey(
	ctx context.Context,
	tenantID, endpoint, key, fingerprint string,
) (*IdempotencyRecord, error) {
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
			ON CONFLICT (tenant_id, endpoint, idempotency_key) DO NOTHING`,
			tenantID, endpoint, key, fingerprint)
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 1 {
			// We hold the claim. Execute.
			return nil
		}

		// Somebody got there first — this run or a previous one.
		var existing IdempotencyRecord
		err = tx.QueryRow(ctx, `
			SELECT request_fingerprint, response_status, response_body, created_at
			  FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
			tenantID, endpoint, key).Scan(
			&existing.RequestFingerprint, &existing.ResponseStatus,
			&existing.ResponseBody, &existing.CreatedAt)
		if err != nil {
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
//
// Called only after the handler has run. A command whose response is never
// recorded leaves status 0 behind, which ReleaseIdempotencyKey cleans up on
// the failure path so a 5xx does not permanently poison the key.
func (s *PgStore) CompleteIdempotencyKey(
	ctx context.Context,
	tenantID, endpoint, key string,
	status int,
	body []byte,
) error {
	if len(body) == 0 {
		// jsonb will not accept an empty string, and a 204 legitimately has no
		// body. 'null' round-trips as "no body" without a second column.
		body = []byte("null")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys
			   SET response_status = $1, response_body = $2
			 WHERE tenant_id = $3 AND endpoint = $4 AND idempotency_key = $5`,
			status, body, tenantID, endpoint, key)
		if err != nil {
			return fmt.Errorf("complete idempotency key: %w", err)
		}
		return nil
	})
}

// ReleaseIdempotencyKey drops a claim whose command did not reach a terminal
// answer.
//
// Server errors are not recorded as the command's outcome. A 503 from a
// dependency is a reason to retry, and a retry that is answered with the stored
// 503 forever would turn a transient upstream blip into a permanently broken
// key — the opposite of what replay protection is for.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3
			   AND response_status = 0`,
			tenantID, endpoint, key)
		if err != nil {
			return fmt.Errorf("release idempotency key: %w", err)
		}
		return nil
	})
}

// PurgeIdempotencyKeysBefore drops recorded responses older than cutoff.
//
// Cross-tenant by construction — a retention sweep has no tenant. Reuses the
// retention sweep's existing hatch rather than minting a third GUC name: this
// runs from the same worker, and the table it touches holds no tenant data
// beyond command responses the tenant itself received.
func (s *PgStore) PurgeIdempotencyKeysBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.retention_sweep', 'true', true)"); err != nil {
		return 0, fmt.Errorf("set_config app.retention_sweep: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge idempotency keys: %w", err)
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}
