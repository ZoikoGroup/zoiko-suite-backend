package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/consolidation-svc/internal/domain"
)

// commandClaim is one row of command_idempotency (migration 000008): the
// command an Idempotency-Key was used for, and the one resource it acted on.
// Exactly one of RunID and AdjustmentID is set, enforced by the table's CHECK.
type commandClaim struct {
	Command      string
	RunID        *string
	AdjustmentID *string
}

// lookupCommandClaim reads key's claim, or nil when the key has never been
// used. For the adjustment commands the caller holds the adjustment's row
// lock, so two concurrent first attempts are serialised and the second sees
// the first's claim. StartRun has no row to lock yet: its claim is protected
// by the table's primary key, and CreateRun retries once on that violation.
func lookupCommandClaim(ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey string) (*commandClaim, error) {
	var c commandClaim
	err := tx.QueryRow(ctx, `
		SELECT command, consolidation_run_id::text, consolidation_adjustment_id::text
		FROM command_idempotency
		WHERE tenant_id = $1 AND idempotency_key = $2
	`, tenantID, idempotencyKey).Scan(&c.Command, &c.RunID, &c.AdjustmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// recordCommandClaim claims key for command on exactly one target, in the same
// transaction as the write it protects: a refused command never consumes its
// key, and a committed command always leaves its claim.
func recordCommandClaim(ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey, command string, runID, adjustmentID *string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO command_idempotency (
			tenant_id, idempotency_key, command, consolidation_run_id, consolidation_adjustment_id
		) VALUES ($1, $2, $3, $4, $5)
	`, tenantID, idempotencyKey, command, runID, adjustmentID)
	return err
}

// commandConflict is a key that cannot replay this command: it was used for a
// different command, or for a different resource.
func commandConflict(idempotencyKey string, c *commandClaim) error {
	return fmt.Errorf("%w: key %q was used for %s", domain.ErrIdempotencyKeyConflict, idempotencyKey, c.Command)
}

// isUniqueViolation reports PostgreSQL's unique-constraint error (23505).
// CreateRun and CreateAdjustment use it to turn a concurrent first attempt
// with one key into a replay: the loser's transaction rolled back, so nothing
// it wrote survives, and the winner's committed claim is readable.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
