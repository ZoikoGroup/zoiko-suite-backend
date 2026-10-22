package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/comments-collaboration-svc/internal/domain"
)

// addCommentResult and reactResult are the stored shapes of the two
// commands whose store result is not a single domain type.
type addCommentResult struct {
	Comment *domain.Comment       `json:"comment"`
	Thread  *domain.CommentThread `json:"thread"`
}

type reactResult struct {
	Added bool `json:"added"`
}

// idemClaim claims (tenant, principal, operation, key) inside the caller's
// transaction, BEFORE the mutation runs.
//
//   - First use: the claim row is inserted and (nil, nil) returned — the
//     caller proceeds and finishes with idemRecord in the same transaction.
//   - Concurrent use: the INSERT blocks on the in-flight transaction's
//     uncommitted claim; once that commits it conflicts, and the stored
//     result is returned. If it rolled back, this call wins the claim.
//   - Replay: the stored result is returned and k.Replayed is set.
//   - Same key, different request: domain.ErrIdempotencyConflict.
//
// Claim and result commit atomically with the mutation, so a crash can
// never leave a claimed key without its result or its rows.
func idemClaim[T any](ctx context.Context, tx pgx.Tx, tenantID string, k *domain.Idempotency) (*T, error) {
	if k == nil {
		return nil, nil
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, principal_id, operation, idempotency_key, request_hash)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, principal_id, operation, idempotency_key) DO NOTHING
	`, tenantID, k.PrincipalID, k.Operation, k.Key, k.RequestHash)
	if err != nil {
		return nil, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var hash string
	var result []byte
	if err := tx.QueryRow(ctx, `
		SELECT request_hash, result FROM idempotency_keys
		WHERE tenant_id=$1 AND principal_id=$2 AND operation=$3 AND idempotency_key=$4
	`, tenantID, k.PrincipalID, k.Operation, k.Key).Scan(&hash, &result); err != nil {
		return nil, fmt.Errorf("read idempotency key: %w", err)
	}
	if hash != k.RequestHash {
		return nil, domain.ErrIdempotencyConflict
	}
	if result == nil {
		return nil, fmt.Errorf("idempotency key %q has no stored result", k.Key)
	}
	var out T
	if err := json.Unmarshal(result, &out); err != nil {
		return nil, fmt.Errorf("decode stored idempotent result: %w", err)
	}
	k.Replayed = true
	return &out, nil
}

// idemRecord stores the command's result against the claim made by
// idemClaim, in the same transaction.
func idemRecord(ctx context.Context, tx pgx.Tx, tenantID string, k *domain.Idempotency, result any) error {
	if k == nil {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode idempotent result: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE idempotency_keys SET result=$5
		WHERE tenant_id=$1 AND principal_id=$2 AND operation=$3 AND idempotency_key=$4
	`, tenantID, k.PrincipalID, k.Operation, k.Key, b); err != nil {
		return fmt.Errorf("record idempotent result: %w", err)
	}
	return nil
}
