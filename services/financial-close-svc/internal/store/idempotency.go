package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// Command names recorded in command_idempotency (migration 000021). A key
// replays only the command it was first used for.
const (
	CommandCreateAccrual        = "CreateAccrual"
	CommandCreatePrepayment     = "CreateRecognitionSchedule"
	CommandCreateAllocationRule = "CreateAllocationRule"
	CommandStartRevaluation     = "StartRevaluation"

	// ACC-07 accrual lifecycle commands (SPEC_DEVIATIONS A11). Each claims
	// the same targetAccrualSchedule column CreateAccrual does; the command
	// name is what disambiguates a key reused for the wrong lifecycle step.
	CommandSubmitAccrual = "SubmitAccrual"
	CommandApproveAccrual = "ApproveAccrual"
	CommandAmendAccrual  = "AmendFutureSchedule"
	CommandCancelAccrual = "CancelAccrual"

	// ACC-08 prepayment lifecycle commands (SPEC_DEVIATIONS A11), the mirror
	// of the ACC-07 set above; each claims targetPrepaymentSchedule.
	CommandApprovePrepayment  = "ApprovePrepayment"
	CommandModifyPrepayment   = "ModifyPrepayment"
	CommandTerminatePrepayment = "TerminatePrepayment"
)

// claimTarget is the command_idempotency column naming what a command
// created. The column is always one of the constants below, never caller
// input, so it is safe to place in SQL text.
type claimTarget string

const (
	targetAccrualSchedule    claimTarget = "accrual_schedule_id"
	targetPrepaymentSchedule claimTarget = "prepayment_schedule_id"
	targetAllocationRule     claimTarget = "allocation_rule_version_id"
	targetFXRevaluationRun   claimTarget = "fx_revaluation_run_id"
)

// commandClaim is one command_idempotency row.
type commandClaim struct {
	Command     string
	RequestHash string
	TargetID    string
}

func lookupCommandClaim(ctx context.Context, tx pgx.Tx, tenantID, idempotencyKey string) (*commandClaim, error) {
	var c commandClaim
	err := tx.QueryRow(ctx, `
		SELECT command, request_hash,
		       COALESCE(accrual_schedule_id, prepayment_schedule_id,
		                allocation_rule_version_id, fx_revaluation_run_id)::text
		FROM command_idempotency
		WHERE tenant_id = $1 AND idempotency_key = $2
	`, tenantID, idempotencyKey).Scan(&c.Command, &c.RequestHash, &c.TargetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// createWithClaim is the platform's command_idempotency rule for a command
// that creates one resource: in one transaction, either replay what an
// earlier use of the key created (load fills the caller's value from that
// resource), or perform insert and record the claim beside it, so a refused
// create never consumes its key and a committed one always leaves its claim.
//
// The same key for a different command or a different request body is
// ErrIdempotencyKeyConflict — it is never answered with another request's
// result. Two concurrent first attempts with one key race on the claim's
// primary key; the loser's transaction rolls back whole and the retry reads
// the winner's committed claim and replays it.
func (s *PgStore) createWithClaim(ctx context.Context, idempotencyKey, command, requestHash string, target claimTarget, targetID string,
	insert func(tx pgx.Tx) error, load func(tx pgx.Tx, id string) error) (replay bool, err error) {
	if idempotencyKey == "" {
		return false, domain.ErrIdempotencyKeyRequired
	}
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	attempt := func() (bool, error) {
		replayed := false
		err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
			claim, err := lookupCommandClaim(ctx, tx, tenantID, idempotencyKey)
			if err != nil {
				return err
			}
			if claim != nil {
				if claim.Command != command || claim.RequestHash != requestHash {
					return fmt.Errorf("%w: key %q was used for %s", domain.ErrIdempotencyKeyConflict, idempotencyKey, claim.Command)
				}
				replayed = true
				return load(tx, claim.TargetID)
			}
			if err := insert(tx); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, fmt.Sprintf(`
				INSERT INTO command_idempotency (tenant_id, idempotency_key, command, request_hash, %s)
				VALUES ($1, $2, $3, $4, $5)`, target), tenantID, idempotencyKey, command, requestHash, targetID)
			return err
		})
		return replayed, err
	}
	replay, err = attempt()
	if isUniqueViolation(err, "command_idempotency_pkey") {
		replay, err = attempt()
	}
	return replay, mapPgError(err)
}

// transitionWithClaim is createWithClaim applied to a command that moves an
// EXISTING resource instead of minting one: targetID is already known (the
// caller supplies it, e.g. from the URL), and perform is the guarded status
// transition rather than an INSERT. The same rules apply: a refused
// transition rolls the whole attempt back and never claims the key; a
// replay loads what the first application left rather than re-running the
// transition. Because the resource id is caller-supplied, not generated,
// requestHash must fold the id in (the handler does this) so the same key
// reused against a different resource is a conflict rather than a silent
// replay of the first one.
func (s *PgStore) transitionWithClaim(ctx context.Context, idempotencyKey, command, requestHash string, target claimTarget, targetID string,
	perform func(tx pgx.Tx) error, load func(tx pgx.Tx, id string) error) (replay bool, err error) {
	return s.createWithClaim(ctx, idempotencyKey, command, requestHash, target, targetID, perform, load)
}

// isUniqueViolation reports PostgreSQL's unique-constraint error (23505) on
// the named constraint, or on any constraint when name is empty.
func isUniqueViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (name == "" || pgErr.ConstraintName == name)
}
