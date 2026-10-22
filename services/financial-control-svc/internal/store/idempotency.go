package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// commandKey is one command_idempotency claim (ZS-CONTROL-001: "Idempotency:
// Mandatory for run creation, execution, reperformance and certification
// commands"): the Idempotency-Key, the command it was used for, and what it
// acted on — the run, and for an exception transition the exception too
// (migration 000008). It is the same table and the same rule BeginExecution
// applies: a repeat of the key for the same command and target is a replay; the
// key used for anything else is a conflict.
type commandKey struct {
	key         string
	command     string
	runID       string
	exceptionID *string
}

// Command names. Each records what was decided, so one key cannot replay a
// different decision on the same target.
func certifyCommand(decision string) string { return decision } // CERTIFY | REJECT

const submitCommand = "SUBMIT_FOR_CERTIFICATION"

func exceptionCommand(to string) string { return "EXCEPTION:" + to }

// lookupCommandKey reports whether k was already claimed. The caller must hold
// the target's row lock, so two concurrent first attempts with one key are
// serialised and the second sees the first's claim as a replay.
func lookupCommandKey(ctx context.Context, tx pgx.Tx, tenantID string, k commandKey) (bool, error) {
	if k.key == "" {
		return false, fmt.Errorf("%w: an Idempotency-Key is required", domain.ErrInvalidArgument)
	}
	var runID, command string
	var exceptionID *string
	err := tx.QueryRow(ctx, `SELECT run_id::text, command, exception_id::text FROM command_idempotency
		WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, k.key).Scan(&runID, &command, &exceptionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	sameException := (exceptionID == nil) == (k.exceptionID == nil) &&
		(exceptionID == nil || *exceptionID == *k.exceptionID)
	if runID != k.runID || command != k.command || !sameException {
		return false, fmt.Errorf("%w: idempotency key already used for a different command", domain.ErrConflict)
	}
	return true, nil
}

// recordCommandKey claims k. It runs after the command succeeded, in its
// transaction, so a refused command never consumes the key.
func recordCommandKey(ctx context.Context, tx pgx.Tx, tenantID string, k commandKey) error {
	_, err := tx.Exec(ctx, `INSERT INTO command_idempotency (tenant_id, idempotency_key, command, run_id, exception_id)
		VALUES ($1,$2,$3,$4,$5)`, tenantID, k.key, k.command, k.runID, k.exceptionID)
	return err
}

// lockRun takes the run's row lock and returns it as it stands.
func lockRun(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.ControlRun, error) {
	var r domain.ControlRun
	if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
		WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, runID), &r); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}
