package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// staleReservationSeconds is how long an IN_PROGRESS reservation is honoured.
// A reservation older than this belongs to a request whose process died
// mid-flight, so a retry may take it over.
const staleReservationSeconds = 60

// IdempotencyOutcome is the result of ReserveIdempotencyKey.
type IdempotencyOutcome int

const (
	// IdempotencyReserved: first use of the key — run the command.
	IdempotencyReserved IdempotencyOutcome = iota
	// IdempotencyReplay: the same request already succeeded — replay it.
	IdempotencyReplay
)

// StoredResponse is the recorded response of a completed command.
type StoredResponse struct {
	Status int
	Body   []byte
}

// ReserveIdempotencyKey claims (tenantID, key) for a request with the given
// fingerprint.
//
//   - first use                      -> IdempotencyReserved
//   - same fingerprint, completed    -> IdempotencyReplay + stored response
//   - same fingerprint, in progress  -> domain.ErrIdempotencyInFlight
//   - different fingerprint          -> domain.ErrIdempotencyMismatch
func (s *PgStore) ReserveIdempotencyKey(ctx context.Context, tenantID, key, fingerprint string) (IdempotencyOutcome, *StoredResponse, error) {
	outcome := IdempotencyReserved
	var stored *StoredResponse

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// Step 1: try to insert a fresh reservation, or take over a stale one.
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_records (tenant_id, idempotency_key, request_fingerprint, state)
			VALUES ($1, $2, $3, 'IN_PROGRESS')
			ON CONFLICT (tenant_id, idempotency_key) DO UPDATE
			   SET request_fingerprint = EXCLUDED.request_fingerprint, created_at = now()
			 WHERE idempotency_records.state = 'IN_PROGRESS'
			   AND idempotency_records.request_fingerprint = EXCLUDED.request_fingerprint
			   AND idempotency_records.created_at < now() - ($4::int * interval '1 second')`,
			tenantID, key, fingerprint, staleReservationSeconds)
		if err != nil {
			return fmt.Errorf("reserve idempotency key: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil // reserved
		}

		// Step 2: the key exists — decide between replay, in-flight, mismatch.
		var (
			existingFingerprint string
			state               string
			status              *int
			body                []byte
		)
		err = tx.QueryRow(ctx, `
			SELECT request_fingerprint, state, response_status, response_body
			FROM idempotency_records WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, key).Scan(&existingFingerprint, &state, &status, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrIdempotencyInFlight // released concurrently; caller may retry
		}
		if err != nil {
			return fmt.Errorf("read idempotency record: %w", err)
		}
		if existingFingerprint != fingerprint {
			return domain.ErrIdempotencyMismatch
		}
		if state != "COMPLETED" || status == nil {
			return domain.ErrIdempotencyInFlight
		}
		outcome = IdempotencyReplay
		stored = &StoredResponse{Status: *status, Body: body}
		return nil
	})
	return outcome, stored, err
}

// CompleteIdempotencyKey records the successful response for later replay.
func (s *PgStore) CompleteIdempotencyKey(ctx context.Context, tenantID, key string, status int, body []byte) error {
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_records
			SET state = 'COMPLETED', response_status = $3, response_body = $4, completed_at = now()
			WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, key, status, body)
		return err
	})
}

// ReleaseIdempotencyKey deletes a reservation whose command did not succeed,
// so the client can correct the request and retry with the same key.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, tenantID, key string) error {
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_records
			WHERE tenant_id = $1 AND idempotency_key = $2 AND state = 'IN_PROGRESS'`,
			tenantID, key)
		return err
	})
}
