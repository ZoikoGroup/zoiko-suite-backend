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
var ErrIdempotencyFingerprintMismatch = errors.New("idempotency key reused with a different request")

// IdempotencyInFlightLease is how long an in-flight claim is honoured before it
// is treated as abandoned. It must comfortably exceed the longest command: the
// HTTP server's WriteTimeout is 30s here, and a claim shorter than a live
// command would let a retry execute alongside it — the duplicate this table
// exists to prevent.
const IdempotencyInFlightLease = 5 * time.Minute

// ClaimIdempotencyKey attempts to reserve (tenant, endpoint, key) for a
// request with this fingerprint.
//
// Ported from identity-context-svc's store, which the Group 1 audit names as
// the pattern to copy, including its two later fixes: the fingerprint is bound
// to the principal (so a second principal replaying the first one's key is a
// mismatch, not a replay of someone else's response), and an in-flight claim
// older than the lease is taken over by an identical request (so a crash
// mid-command does not lock the caller out until the retention purge).
//
// Returns (nil, nil) when the claim succeeded and the caller should execute.
// Returns a record when the key was seen with the SAME fingerprint — a retry,
// to be answered from the record. Returns ErrIdempotencyFingerprintMismatch
// when it was seen with a different one.
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
func (s *PgStore) CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	if len(body) == 0 {
		// jsonb will not accept an empty string; 'null' round-trips as "no body".
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
// answer, so a 5xx does not become a permanently broken key.
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
// Platform-scoped: a retention sweep has no tenant.
func (s *PgStore) PurgeIdempotencyKeysBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, cutoff)
		if err != nil {
			return fmt.Errorf("purge idempotency keys: %w", err)
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}
