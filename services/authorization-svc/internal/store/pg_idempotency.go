package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── idempotency keys (000021) ───────────────────────────────────────────────
//
// The same algorithm as identity-context-svc's pg_idempotency.go and
// access-control-svc's pg_governance.go.

// IdempotencyRecord is a command's first response.
type IdempotencyRecord struct {
	RequestFingerprint string
	ResponseStatus     int
	ResponseBody       []byte
	CreatedAt          time.Time
}

// ErrIdempotencyFingerprintMismatch: the key was used for a different request.
var ErrIdempotencyFingerprintMismatch = errors.New("idempotency key reused with a different request")

// IdempotencyInFlightLease is how long an unfinished claim is honoured before
// an identical retry may take it over. Well above the server's write timeout.
const IdempotencyInFlightLease = 5 * time.Minute

// ClaimIdempotencyKey reserves (tenant, endpoint, key). (nil, nil): execute.
// A record: a genuine retry, answer from it (status 0 = still in flight).
// ErrIdempotencyFingerprintMismatch: the key belongs to another request.
func (s *PgStore) ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*IdempotencyRecord, error) {
	if tenantID == "" || endpoint == "" || key == "" {
		return nil, errors.New("ClaimIdempotencyKey: tenant_id, endpoint and idempotency_key are required")
	}
	var rec *IdempotencyRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint, response_status, response_body)
			VALUES ($1,$2,$3,$4,0,'null'::jsonb)
			ON CONFLICT (tenant_id, endpoint, idempotency_key) DO UPDATE SET created_at = now()
			 WHERE idempotency_keys.response_status = 0
			   AND idempotency_keys.created_at < now() - make_interval(secs => $5)
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
			  FROM idempotency_keys WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
			tenantID, endpoint, key).Scan(&existing.RequestFingerprint, &existing.ResponseStatus, &existing.ResponseBody, &existing.CreatedAt); err != nil {
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
		body = []byte("null")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET response_status = $1, response_body = $2
			 WHERE tenant_id = $3 AND endpoint = $4 AND idempotency_key = $5`,
			status, body, tenantID, endpoint, key)
		return err
	})
}

// ReleaseIdempotencyKey drops a claim whose command ended in a 5xx, so a
// retry can genuinely retry.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3 AND response_status = 0`,
			tenantID, endpoint, key)
		return err
	})
}
