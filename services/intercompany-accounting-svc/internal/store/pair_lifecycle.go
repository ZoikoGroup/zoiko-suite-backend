package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// Every change to a pair's match_status is recorded, in the transaction that
// makes it, as one row of intercompany_pair_transitions (migration 000006).
// The table is append-only, enforced by trigger, so the history cannot be
// edited after the fact.

// lockPair reads a pair's current status and period under a row lock, so
// the transition recorded below names the state the pair actually left.
func lockPair(ctx context.Context, tx pgx.Tx, tenantID, id string) (status string, fiscalPeriod *string, err error) {
	err = tx.QueryRow(ctx, `
		SELECT match_status, fiscal_period FROM intercompany_entries
		WHERE intercompany_entry_id = $1 AND tenant_id = $2
		FOR UPDATE
	`, id, tenantID).Scan(&status, &fiscalPeriod)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, domain.ErrEntryNotFound
	}
	return status, fiscalPeriod, err
}

// transition is one state change to record.
type transition struct {
	from          *string // nil only for a pair's creation
	to            string
	fiscalPeriod  *string
	reason        *string
	outcome       *string
	actor         string
	correlationID string
}

func recordTransition(ctx context.Context, tx pgx.Tx, tenantID, id string, t transition) error {
	var correlationID *string
	if t.correlationID != "" {
		correlationID = &t.correlationID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO intercompany_pair_transitions
			(tenant_id, intercompany_entry_id, from_state, to_state, fiscal_period, reason, resolution_outcome, actor, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, tenantID, id, t.from, t.to, t.fiscalPeriod, t.reason, t.outcome, t.actor, correlationID); err != nil {
		return fmt.Errorf("record pair transition: %w", err)
	}
	return nil
}

// moveLifecycle moves a pair from one of allowedFrom to t.to: it locks the
// row, refuses any other current status with ErrInvalidPairTransition,
// applies set (an UPDATE that must also carry the status guard, as defence
// in depth), and records the transition, all in one transaction.
func (s *PgStore) moveLifecycle(ctx context.Context, id string, allowedFrom []string, t transition,
	set func(tx pgx.Tx, tenantID string, now time.Time) (int64, error)) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		current, period, err := lockPair(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !slices.Contains(allowedFrom, current) {
			return domain.ErrInvalidPairTransition
		}
		n, err := set(tx, tenantID, time.Now().UTC())
		if err != nil {
			return err
		}
		if n == 0 {
			return domain.ErrInvalidPairTransition
		}
		t.from, t.fiscalPeriod = &current, period
		return recordTransition(ctx, tx, tenantID, id, t)
	})
}

// AcknowledgeCounterparty is ACC-11's own AcknowledgeCounterparty command
// — moves a pair from OPEN ("UNMATCHED") to AWAITING_COUNTERPARTY. Guarded
// to only ever succeed from UNMATCHED, matching the spec's own state model
// order.
func (s *PgStore) AcknowledgeCounterparty(ctx context.Context, id, principalID, correlationID string) error {
	return s.moveLifecycle(ctx, id, []string{domain.MatchStatusUnmatched},
		transition{to: domain.MatchStatusAwaitingCounterparty, actor: principalID, correlationID: correlationID},
		func(tx pgx.Tx, tenantID string, now time.Time) (int64, error) {
			res, err := tx.Exec(ctx, `
				UPDATE intercompany_entries
				SET match_status = $1, acknowledged_at = $2, acknowledged_by_principal_id = $3, updated_at = $2
				WHERE intercompany_entry_id = $4 AND tenant_id = $5 AND match_status = $6
			`, domain.MatchStatusAwaitingCounterparty, now, principalID, id, tenantID, domain.MatchStatusUnmatched)
			return res.RowsAffected(), err
		})
}

// DisputeIntercompany is ACC-11's own DisputeIntercompany command — moves
// a pair from MISMATCH to DISPUTED. Only reachable from MISMATCH: there
// is nothing to dispute about a pair that already matched or was never
// even checked.
func (s *PgStore) DisputeIntercompany(ctx context.Context, id, principalID, reason, correlationID string) error {
	return s.moveLifecycle(ctx, id, []string{domain.MatchStatusMismatch},
		transition{to: domain.MatchStatusDisputed, reason: &reason, actor: principalID, correlationID: correlationID},
		func(tx pgx.Tx, tenantID string, now time.Time) (int64, error) {
			res, err := tx.Exec(ctx, `
				UPDATE intercompany_entries
				SET match_status = $1, disputed_at = $2, disputed_by_principal_id = $3, dispute_reason = $4, updated_at = $2
				WHERE intercompany_entry_id = $5 AND tenant_id = $6 AND match_status = $7
			`, domain.MatchStatusDisputed, now, principalID, reason, id, tenantID, domain.MatchStatusMismatch)
			return res.RowsAffected(), err
		})
}

// ResolveMismatch is ACC-11's own ResolveMismatch command — moves a pair
// from DISPUTED to RESOLVED, recording the outcome (accepted, corrected or
// written off) alongside the note. Never re-runs matching itself: a
// resolution is a human decision recorded as evidence, not an automatic
// recheck.
func (s *PgStore) ResolveMismatch(ctx context.Context, id, principalID, resolutionNote, outcome, correlationID string) error {
	if !domain.ValidResolutionOutcome(outcome) {
		return domain.ErrResolutionOutcomeInvalid
	}
	return s.moveLifecycle(ctx, id, []string{domain.MatchStatusDisputed},
		transition{to: domain.MatchStatusResolved, reason: &resolutionNote, outcome: &outcome, actor: principalID, correlationID: correlationID},
		func(tx pgx.Tx, tenantID string, now time.Time) (int64, error) {
			res, err := tx.Exec(ctx, `
				UPDATE intercompany_entries
				SET match_status = $1, resolved_at = $2, resolved_by_principal_id = $3, resolution_note = $4,
				    resolution_outcome = $5, updated_at = $2
				WHERE intercompany_entry_id = $6 AND tenant_id = $7 AND match_status = $8
			`, domain.MatchStatusResolved, now, principalID, resolutionNote, outcome, id, tenantID, domain.MatchStatusDisputed)
			return res.RowsAffected(), err
		})
}

// ClosePair moves a MATCHED pair to CLOSED, its terminal state (e.g. once
// its period has been consolidated). Only MATCHED may close: a pair still
// open, mismatched or disputed has an unsettled difference, and closing it
// would take that difference out of view ("Unmatched intercompany balances
// remain visible and are not silently eliminated").
func (s *PgStore) ClosePair(ctx context.Context, id, principalID, reason, correlationID string) error {
	return s.moveLifecycle(ctx, id, []string{domain.MatchStatusMatched},
		transition{to: domain.MatchStatusClosed, reason: &reason, actor: principalID, correlationID: correlationID},
		func(tx pgx.Tx, tenantID string, now time.Time) (int64, error) {
			res, err := tx.Exec(ctx, `
				UPDATE intercompany_entries
				SET match_status = $1, closed_at = $2, closed_by_principal_id = $3, close_reason = $4, updated_at = $2
				WHERE intercompany_entry_id = $5 AND tenant_id = $6 AND match_status = $7
			`, domain.MatchStatusClosed, now, principalID, reason, id, tenantID, domain.MatchStatusMatched)
			return res.RowsAffected(), err
		})
}

// ListTransitions returns a pair's state history, oldest first.
func (s *PgStore) ListTransitions(ctx context.Context, id string) ([]domain.PairTransition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	out := []domain.PairTransition{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT transition_id, intercompany_entry_id::text, from_state, to_state, fiscal_period, reason,
			       resolution_outcome, actor, correlation_id, occurred_at
			FROM intercompany_pair_transitions
			WHERE tenant_id = $1 AND intercompany_entry_id = $2
			ORDER BY transition_id
		`, tenantID, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.PairTransition
			if err := rows.Scan(&t.TransitionID, &t.IntercompanyEntryID, &t.FromState, &t.ToState, &t.FiscalPeriod,
				&t.Reason, &t.ResolutionOutcome, &t.Actor, &t.CorrelationID, &t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
