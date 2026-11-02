package store

import (
	"context"
	"fmt"
	"time"
)

// IdempotencyRecord is the stored outcome of a write that ran under an
// Idempotency-Key. A retry with the same key and the same request replays
// ResponseBody instead of executing again (INV-08).
type IdempotencyRecord struct {
	Key            string
	Operation      string
	RequestHash    string
	ResponseStatus int
	ResponseBody   []byte
	ResponseETag   string
	CreatedAt      time.Time
}

// FindIdempotencyRecord returns the record for key, locked for the rest of
// the transaction, or ErrNotFound.
func (q *TenantQueries) FindIdempotencyRecord(ctx context.Context, key string) (*IdempotencyRecord, error) {
	var record IdempotencyRecord
	var etag *string
	err := q.tx.QueryRow(ctx, `
		SELECT idempotency_key, operation, request_hash, response_status, response_body::text, response_etag, created_at
		FROM risk_idempotency_records
		WHERE tenant_id = $1 AND idempotency_key = $2
		FOR UPDATE`, q.TenantID, key).Scan(
		&record.Key, &record.Operation, &record.RequestHash, &record.ResponseStatus,
		&record.ResponseBody, &etag, &record.CreatedAt,
	)
	if err != nil {
		return nil, translatePgError(err)
	}
	if etag != nil {
		record.ResponseETag = *etag
	}
	return &record, nil
}

// SaveIdempotencyRecord stores the outcome of a successful write. If a
// concurrent request already stored the same key, the unique violation
// surfaces at commit as *ErrUniqueViolation and this transaction rolls back.
func (q *TenantQueries) SaveIdempotencyRecord(ctx context.Context, record IdempotencyRecord) error {
	var etag *string
	if record.ResponseETag != "" {
		etag = &record.ResponseETag
	}
	_, err := q.tx.Exec(ctx, `
		INSERT INTO risk_idempotency_records (
			tenant_id, idempotency_key, operation, request_hash, response_status, response_body, response_etag, created_at
		) VALUES ($1, $2, $3, $4, $5, $6::json, $7, $8)`,
		q.TenantID, record.Key, record.Operation, record.RequestHash, record.ResponseStatus,
		string(record.ResponseBody), etag, record.CreatedAt,
	)
	if err != nil {
		return translatePgError(fmt.Errorf("save idempotency record: %w", err))
	}
	return nil
}
