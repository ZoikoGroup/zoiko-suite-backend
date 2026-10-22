package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"zoiko.io/audit-event-store-svc/internal/domain"
)

// claimIdempotency claims (tenant, endpoint, key) inside CreateArchive's own
// transaction, BEFORE the archive is built.
//
//   - First use: the claim row is inserted and (nil, nil) is returned; the
//     caller proceeds and finishes with recordIdempotency in the same
//     transaction.
//   - Concurrent duplicate: the INSERT blocks on the in-flight transaction's
//     uncommitted claim. When that commits, this INSERT conflicts and the
//     stored archive is returned; if it rolled back, this call wins the claim.
//   - Replay: the stored archive is returned and k.Replayed is set.
//   - Same key, different request (fingerprint — principal and range — differs):
//     domain.ErrIdempotencyMismatch, never the other request's archive.
//
// The caller's tenant scope (app.tenant_id) is already set on tx, so the
// tenant-only RLS policy applies: a key is only ever visible to its own tenant.
func claimIdempotency(ctx context.Context, tx pgx.Tx, tenantID string, k *domain.Idempotency) (*domain.Archive, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, endpoint, idempotency_key) DO NOTHING`,
		tenantID, k.Endpoint, k.Key, k.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}

	var fingerprint string
	var status int
	var body []byte
	if err := tx.QueryRow(ctx, `
		SELECT request_fingerprint, response_status, response_body
		  FROM idempotency_keys
		 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
		tenantID, k.Endpoint, k.Key).Scan(&fingerprint, &status, &body); err != nil {
		return nil, fmt.Errorf("read idempotency key: %w", err)
	}
	if fingerprint != k.Fingerprint {
		return nil, domain.ErrIdempotencyMismatch
	}
	if status == 0 {
		// Cannot happen: claim and response commit together. Refuse rather
		// than guess a response.
		return nil, fmt.Errorf("idempotency key has no recorded response")
	}
	var a domain.Archive
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("decode recorded archive: %w", err)
	}
	k.Replayed = true
	return &a, nil
}

// recordIdempotency stores the archive created under a claimed key, in the
// same transaction as the archive row.
func recordIdempotency(ctx context.Context, tx pgx.Tx, tenantID string, k *domain.Idempotency, a *domain.Archive) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode archive for idempotency record: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		   SET response_status = $4, response_body = $5
		 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
		tenantID, k.Endpoint, k.Key, http.StatusCreated, body); err != nil {
		return fmt.Errorf("record idempotent response: %w", err)
	}
	return nil
}
