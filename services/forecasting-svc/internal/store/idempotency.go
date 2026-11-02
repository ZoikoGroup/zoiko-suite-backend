package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/forecasting-svc/internal/domain"
)

// idempotencyOwnerScope fills the owner_scope column of idempotency_keys.
const idempotencyOwnerScope = "forecasting-svc"

// claimIdempotencyKey reserves the caller's Idempotency-Key inside the current
// transaction, BEFORE any business rule is checked.
//
// Outcomes:
//   - nil: first use of the key; the command proceeds and must call
//     recordIdempotentResult before committing.
//   - *domain.IdempotentReplay: the same key and the same request fingerprint
//     already succeeded; the caller returns that resource unchanged.
//   - domain.ErrIdempotencyKeyReused: the key was used for a different request.
//
// Concurrency: INSERT ... ON CONFLICT DO NOTHING blocks while another
// transaction holds the same key, so two concurrent identical requests
// serialise and the second sees the first one's committed result.
func claimIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID string, claim domain.IdempotencyClaim) error {
	if claim.Key == "" {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES ($1, $2, $3, $4, $5, $6, '')
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		tenantID, idempotencyOwnerScope, claim.PrincipalID, claim.Key, claim.Operation, claim.RequestSHA256)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	var storedFingerprint, storedResourceID string
	err = tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = $1 AND owner_scope = $2 AND principal_id = $3 AND idempotency_key = $4`,
		tenantID, idempotencyOwnerScope, claim.PrincipalID, claim.Key).Scan(&storedFingerprint, &storedResourceID)
	if err != nil {
		return fmt.Errorf("read idempotency key: %w", err)
	}
	if storedFingerprint != claim.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	if storedResourceID == "" {
		return domain.NewConflictError("idempotency_in_progress", "a request with this Idempotency-Key is still being processed")
	}
	return &domain.IdempotentReplay{ResourceID: storedResourceID}
}

// recordIdempotentResult stores the id of the resource the command produced so
// a later replay can return it.
func recordIdempotentResult(ctx context.Context, tx pgx.Tx, tenantID string, claim domain.IdempotencyClaim, resourceID string) error {
	if claim.Key == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE idempotency_keys SET resource_id = $5
		WHERE tenant_id = $1 AND owner_scope = $2 AND principal_id = $3 AND idempotency_key = $4`,
		tenantID, idempotencyOwnerScope, claim.PrincipalID, claim.Key, resourceID)
	if err != nil {
		return fmt.Errorf("record idempotent result: %w", err)
	}
	return nil
}
