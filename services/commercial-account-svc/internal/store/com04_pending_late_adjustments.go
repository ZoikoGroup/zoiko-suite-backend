// COM-04 durable late-event inbox (migration 000025; SPEC_DEVIATIONS A8).
//
// registerLateEvent (com04_usage_store.go) queues a pending_late_adjustments
// row instead of failing when no target statement exists yet for a late
// event. ProcessNextPendingLateAdjustment is the worker that drains that
// queue: shaped exactly like ProcessNextEntitlementInvalidation
// (com03_entitlement_store.go), itself shaped like subscription_boundary_queue's
// ProcessNextBoundary (internal/boundary) -- one claimed item per
// transaction, FOR UPDATE SKIP LOCKED so any number of replicas can run this
// safely side by side.
//
// Unlike those two queues, there is no terminal FAILED/expiry state here: the
// governing spec gives no expiry for a late correction (§4.4, §9 negative
// path #18 says only "create prospective adjustment", never "until X"), so
// an item with no resolvable target yet -- including a subscription that has
// permanently ended with no further term -- stays PENDING and keeps being
// retried on a capped backoff, forever, rather than being discarded or
// silently dropped. A genuine unexpected error (a database failure, not
// merely "no target yet") is recorded the same way and still retried, never
// parked: the queue's only two states are "not yet attached" and "attached".
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
)

const pendingLateAdjustmentWorkerActor = "system:pending-late-adjustment-worker"

// pendingLateAdjustmentBackoff doubles from one minute up to an hour, the
// same shape as entitlementInvalidationBackoff/boundaryBackoff -- capped,
// never parked, since this queue never gives up on an item.
func pendingLateAdjustmentBackoff(attempts int) time.Duration {
	d := time.Minute << attempts
	if d > time.Hour || d <= 0 {
		return time.Hour
	}
	return d
}

type pendingLateAdjustmentItem struct {
	id                 string
	org                string
	subscriptionID     string
	meterKey           string
	meterVersion       int
	originStatementID  string
	sourceUsageEventID string
	quantity           string
	occurredAt         time.Time
	dimensions         []byte
	reason             string
	attempts           int
}

// ProcessNextPendingLateAdjustment claims one due pending_late_adjustments
// row and attempts to attach it: it re-resolves a target statement at `now`
// (not the original observedAt, so a term that has opened SINCE the item was
// queued is found), and on success creates the real usage_adjustments row
// through the exact same createLateAdjustment logic registerLateEvent uses
// when a target is available immediately. It reports whether an item was
// found; "no target yet" is recorded as a retry (backoff), never a terminal
// failure.
func (s *PgStore) ProcessNextPendingLateAdjustment(ctx context.Context, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'boundary_worker', true)"); err != nil {
		return false, err
	}

	var it pendingLateAdjustmentItem
	err = tx.QueryRow(ctx, `
		SELECT pending_id::text, organization_id::text, subscription_id, meter_key, meter_version,
		       origin_statement_id, source_usage_event_id, quantity::text, occurred_at, dimensions, reason, attempts
		FROM pending_late_adjustments
		WHERE status = 'PENDING' AND next_attempt_at <= $1
		ORDER BY next_attempt_at, pending_id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&it.id, &it.org, &it.subscriptionID, &it.meterKey, &it.meterVersion,
		&it.originStatementID, &it.sourceUsageEventID, &it.quantity, &it.occurredAt, &it.dimensions, &it.reason, &it.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if perr := s.attachPendingLateAdjustment(ctx, tx, it, now); perr == nil {
		if perr = tx.Commit(ctx); perr == nil {
			return true, nil
		}
		_ = tx.Rollback(ctx)
		if rerr := s.recordPendingLateAdjustmentRetry(ctx, it, perr, now); rerr != nil {
			return true, fmt.Errorf("pending late adjustment %s failed (%v) and the retry could not be recorded: %w", it.id, perr, rerr)
		}
		return true, fmt.Errorf("pending late adjustment %s: %w", it.id, perr)
	} else if errors.Is(perr, domain.ErrStatementInvalidState) || errors.Is(perr, domain.ErrStatementNotOpenForWindow) || errors.Is(perr, domain.ErrNoOpenTermForCorrection) {
		// Still no valid target: not an error, just not due yet. Roll back
		// this attempt's transaction (it claimed nothing durable) and record
		// the retry in its own transaction.
		_ = tx.Rollback(ctx)
		if rerr := s.recordPendingLateAdjustmentRetry(ctx, it, perr, now); rerr != nil {
			return true, fmt.Errorf("pending late adjustment %s still has no target and the retry could not be recorded: %w", it.id, rerr)
		}
		return true, nil
	} else {
		_ = tx.Rollback(ctx)
		if rerr := s.recordPendingLateAdjustmentRetry(ctx, it, perr, now); rerr != nil {
			return true, fmt.Errorf("pending late adjustment %s failed (%v) and the retry could not be recorded: %w", it.id, perr, rerr)
		}
		return true, fmt.Errorf("pending late adjustment %s: %w", it.id, perr)
	}
}

// attachPendingLateAdjustment is ProcessNextPendingLateAdjustment's body
// inside the claimed transaction: resolve a target at `now`, create the
// adjustment, mark the pending row ATTACHED. Returns
// ErrStatementInvalidState/ErrStatementNotOpenForWindow/ErrNoOpenTermForCorrection
// unchanged when there is still no target -- the caller treats those as a
// retry, not a failure.
func (s *PgStore) attachPendingLateAdjustment(ctx context.Context, tx pgx.Tx, it pendingLateAdjustmentItem, now time.Time) error {
	if err := declareOrgForSellerPlane(ctx, tx, it.org); err != nil {
		return err
	}
	nowTermNo, terr := resolveTerm(ctx, tx, it.subscriptionID, now)
	if terr != nil {
		return terr
	}
	m, err := loadMeterDefinition(ctx, tx, it.meterKey, it.meterVersion)
	if err != nil {
		return err
	}
	target, err := findOrCreateOpenStatement(ctx, tx, it.subscriptionID, nowTermNo, *m, now)
	if err != nil {
		return err
	}
	in := domain.EventInput{Quantity: it.quantity, OccurredAt: it.occurredAt}
	adjID, err := s.createLateAdjustment(ctx, tx, it.org, it.originStatementID, target.StatementID, it.meterKey,
		it.sourceUsageEventID, in, it.dimensions, now, it.reason)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE pending_late_adjustments SET status = 'ATTACHED', attached_at = $2,
		attached_adjustment_id = $3 WHERE pending_id = $1`, it.id, now, adjID)
	return err
}

// recordPendingLateAdjustmentRetry records attempts/last_error/next_attempt_at
// for an item that is still PENDING, in its own transaction (the claiming
// transaction above was rolled back). Never moves the item to a terminal
// state: see this file's package comment for why.
func (s *PgStore) recordPendingLateAdjustmentRetry(ctx context.Context, it pendingLateAdjustmentItem, cause error, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'boundary_worker', true)"); err != nil {
		return err
	}
	attempts := it.attempts + 1
	if _, err := tx.Exec(ctx, `UPDATE pending_late_adjustments SET attempts = $2, last_error = $3, next_attempt_at = $4
		WHERE pending_id = $1`, it.id, attempts, cause.Error(), now.Add(pendingLateAdjustmentBackoff(attempts))); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetPendingLateAdjustments is read-only evidence for an organization's own
// queued-but-not-yet-attached late adjustments (and the attached ones, for
// completeness) -- the same "the fact is never silently lost" guarantee
// GetLateEvents already gives usage_event_records.
func (s *PgStore) GetPendingLateAdjustments(ctx context.Context, subscriptionID string) ([]domain.PendingLateAdjustment, error) {
	var out []domain.PendingLateAdjustment
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT pending_id::text, organization_id::text, subscription_id, meter_key,
			meter_version, origin_statement_id, origin_term_no, source_usage_event_id, quantity::text, occurred_at,
			dimensions, reason, created_at, attempts, last_error, next_attempt_at, status, attached_at, attached_adjustment_id
			FROM pending_late_adjustments WHERE subscription_id = $1 ORDER BY created_at`, subscriptionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p domain.PendingLateAdjustment
			var dimJSON []byte
			if err := rows.Scan(&p.PendingID, &p.OrganizationID, &p.SubscriptionID, &p.MeterKey, &p.MeterVersion,
				&p.OriginStatementID, &p.OriginTermNo, &p.SourceUsageEventID, &p.Quantity, &p.OccurredAt, &dimJSON,
				&p.Reason, &p.CreatedAt, &p.Attempts, &p.LastError, &p.NextAttemptAt, &p.Status, &p.AttachedAt,
				&p.AttachedAdjustmentID); err != nil {
				return err
			}
			if len(dimJSON) > 0 {
				if err := json.Unmarshal(dimJSON, &p.Dimensions); err != nil {
					return fmt.Errorf("decode dimensions: %w", err)
				}
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}
