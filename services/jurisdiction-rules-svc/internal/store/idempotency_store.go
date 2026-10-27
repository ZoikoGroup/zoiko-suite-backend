package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// IdempotencyRecord is a stored Idempotency-Key and, once the first request
// finished, its response. Status 0 means that request is still in flight.
type IdempotencyRecord struct {
	RequestHash string
	Status      int
	ContentType string
	Body        []byte
}

// idempotencyRetention is how long a key holds its response; an older row is
// taken over by the next request that names the key.
const idempotencyRetention = "24 hours"

// ClaimIdempotencyKey records the key for this request. It returns
// (nil, nil) when the caller now owns the key and must run the request, or
// the existing record when the key is already held — the caller compares
// request hashes and replays or refuses.
func (s *PgStore) ClaimIdempotencyKey(ctx context.Context, principalID, key, method, path, requestHash string) (*IdempotencyRecord, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (principal_id, idempotency_key, method, path, request_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (principal_id, idempotency_key, method, path) DO UPDATE
		   SET request_hash = EXCLUDED.request_hash, response_status = NULL,
		       content_type = NULL, response_body = NULL, created_at = NOW()
		 WHERE idempotency_keys.created_at < NOW() - INTERVAL '`+idempotencyRetention+`'`,
		principalID, key, method, path, requestHash)
	if err != nil {
		s.log.Error("pg ClaimIdempotencyKey failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}

	var rec IdempotencyRecord
	var status *int
	var contentType *string
	err = s.pool.QueryRow(ctx, `
		SELECT request_hash, response_status, content_type, response_body
		FROM idempotency_keys
		WHERE principal_id = $1 AND idempotency_key = $2 AND method = $3 AND path = $4`,
		principalID, key, method, path).Scan(&rec.RequestHash, &status, &contentType, &rec.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		// Released between our INSERT and this read; the retry owns it.
		return s.ClaimIdempotencyKey(ctx, principalID, key, method, path, requestHash)
	}
	if err != nil {
		s.log.Error("pg ClaimIdempotencyKey read failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if status != nil {
		rec.Status = *status
	}
	if contentType != nil {
		rec.ContentType = *contentType
	}
	return &rec, nil
}

// CompleteIdempotencyKey stores the response of the request that owns the key.
func (s *PgStore) CompleteIdempotencyKey(ctx context.Context, principalID, key, method, path string, status int, contentType string, body []byte) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET response_status = $5, content_type = $6, response_body = $7
		WHERE principal_id = $1 AND idempotency_key = $2 AND method = $3 AND path = $4`,
		principalID, key, method, path, status, contentType, body)
	if err != nil {
		s.log.Error("pg CompleteIdempotencyKey failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ReleaseIdempotencyKey forgets the key, so a request that failed in a way
// worth retrying (5xx) can be retried under the same key.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, principalID, key, method, path string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE principal_id = $1 AND idempotency_key = $2 AND method = $3 AND path = $4`,
		principalID, key, method, path)
	if err != nil {
		s.log.Error("pg ReleaseIdempotencyKey failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}
