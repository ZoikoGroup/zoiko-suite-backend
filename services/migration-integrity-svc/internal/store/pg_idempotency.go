package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── Idempotency-Key replay store ─────────────────────────────────────────────
//
// Protocol (INV-08, MIG-INV-24):
//
//  1. Before the write, the handler calls FindIdempotentResponse. A stored
//     response with the same request hash is replayed; a different hash is a
//     409 (the key was reused for a different request).
//  2. Inside the write transaction, claimIdempotencyKey inserts the key
//     first. A concurrent request with the same key blocks on that insert
//     until the first commits, then fails with a unique violation and gets
//     ErrIdempotencyKeyInUse — the handler then replays the winner's response.
//  3. Just before commit, completeIdempotencyKey stores the response.
//
// Failed requests roll the claim back with everything else, so only a
// successful response is ever replayed and a corrected retry can reuse the key.

const uniqueViolation = "23505"

// FindIdempotentResponse implements Store.
func (s *PgStore) FindIdempotentResponse(ctx context.Context, tenantID, principalID, key string) (*StoredResponse, error) {
	var stored *StoredResponse
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var response StoredResponse
		var body string
		err := tx.QueryRow(ctx, `
			SELECT operation, request_sha256, response_status, response_body
			  FROM migration_idempotency_keys
			 WHERE tenant_id = $1 AND principal_id = $2 AND idempotency_key = $3`,
			tenantID, principalID, key).Scan(&response.Operation, &response.RequestHash, &response.Status, &body)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read idempotency key: %w", err)
		}
		response.Body = []byte(body)
		stored = &response
		return nil
	})
	return stored, err
}

// claimIdempotencyKey reserves the claim's key inside tx (step 2 above).
func claimIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID string, claim IdempotencyClaim) error {
	if claim.IsZero() {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO migration_idempotency_keys
		  (tenant_id, principal_id, idempotency_key, operation, request_sha256)
		VALUES ($1, $2, $3, $4, $5)`,
		tenantID, claim.PrincipalID, claim.Key, claim.Operation, claim.RequestHash)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return ErrIdempotencyKeyInUse
	}
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	return nil
}

// completeIdempotencyKey stores the response for replay (step 3 above). The
// response is marshalled exactly as the handler marshals it, so a replay is
// byte-identical to the first response.
func completeIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID string, claim IdempotencyClaim, response any) error {
	if claim.IsZero() {
		return nil
	}
	body, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshal idempotent response: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE migration_idempotency_keys
		   SET response_status = $4, response_body = $5
		 WHERE tenant_id = $1 AND principal_id = $2 AND idempotency_key = $3`,
		tenantID, claim.PrincipalID, claim.Key, claim.ResponseStatus, string(body))
	if err != nil {
		return fmt.Errorf("store idempotent response: %w", err)
	}
	return nil
}
