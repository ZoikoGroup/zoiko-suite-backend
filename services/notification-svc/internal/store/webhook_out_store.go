package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/events"
	"zoiko.io/notification-svc/internal/whout"
)

// This file is the PostgreSQL implementation of whout.Store (INT-04 outbound
// webhooks). Every method except ClaimDue runs in a tenant-scoped transaction under
// the service's row-level security; ClaimDue names itself as the dispatcher.

var _ whout.Store = (*PgStore)(nil)

const subCols = `subscription_id::text, tenant_id, legal_entity_id, name, destination_url, event_types, allowed_fields,
	api_version, signing_key_path, COALESCE(previous_signing_key_path, ''), previous_key_valid_until, destination_region,
	max_attempts, status, status_reason, version, config_fingerprint, COALESCE(approved_fingerprint, ''), created_by,
	last_changed_by, COALESCE(approved_by, ''), approved_at, created_at, updated_at, consecutive_failures,
	consecutive_dead_letters, breaker_open_until, last_success_at, last_failure_at`

func scanSub(r pgx.Row) (*whout.Subscription, error) {
	var s whout.Subscription
	var st string
	err := r.Scan(&s.SubscriptionID, &s.TenantID, &s.LegalEntityID, &s.Name, &s.DestinationURL, &s.EventTypes, &s.AllowedFields,
		&s.APIVersion, &s.SigningKeyPath, &s.PreviousSigningKeyPath, &s.PreviousKeyValidUntil, &s.DestinationRegion,
		&s.MaxAttempts, &st, &s.StatusReason, &s.Version, &s.ConfigFingerprint, &s.ApprovedFingerprint, &s.CreatedBy,
		&s.LastChangedBy, &s.ApprovedBy, &s.ApprovedAt, &s.CreatedAt, &s.UpdatedAt, &s.ConsecutiveFailures,
		&s.ConsecutiveDeadLetters, &s.BreakerOpenUntil, &s.LastSuccessAt, &s.LastFailureAt)
	if err != nil {
		return nil, err
	}
	s.Status = whout.Status(st)
	return &s, nil
}

const deliveryCols = `delivery_id::text, tenant_id, subscription_id::text, event_id, event_type, event_occurred_at, body, body_hash,
	scope_fingerprint, status, attempt_count, next_attempt_at, locked_until, last_attempt_at, last_response_code,
	COALESCE(last_error_class, ''), COALESCE(dead_letter_reason, ''), dead_lettered_at, succeeded_at, replay_count,
	budget_base, correlation_id, created_at`

func scanDelivery(r pgx.Row) (*whout.Delivery, error) {
	var d whout.Delivery
	var st string
	err := r.Scan(&d.DeliveryID, &d.TenantID, &d.SubscriptionID, &d.EventID, &d.EventType, &d.EventOccurredAt, &d.Body, &d.BodyHash,
		&d.ScopeFingerprint, &st, &d.AttemptCount, &d.NextAttemptAt, &d.LockedUntil, &d.LastAttemptAt, &d.LastResponseCode,
		&d.LastErrorClass, &d.DeadLetterReason, &d.DeadLetteredAt, &d.SucceededAt, &d.ReplayCount,
		&d.BudgetBase, &d.CorrelationID, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	d.Status = whout.DeliveryStatus(st)
	return &d, nil
}

func constraintErr(err error) error {
	switch pgCode(err) {
	case "23514", "23505", "23000", "23502":
		return fmt.Errorf("%w: %v", whout.ErrInvalidState, err)
	}
	return err
}

func insertLog(ctx context.Context, tx pgx.Tx, e whout.LogEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO webhook_subscription_log (tenant_id, subscription_id, kind, actor, approver, approval_ref,
			from_status, to_status, version, fingerprint, delivery_id, reason, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		e.TenantID, e.SubscriptionID, string(e.Kind), e.Actor, nullStr(e.Approver), nullStr(e.ApprovalRef),
		nullStr(string(e.FromStatus)), nullStr(string(e.ToStatus)), e.Version, nullStr(e.Fingerprint),
		nullStr(e.DeliveryID), e.Reason, e.CorrelationID)
	return err
}

// ── subscriptions ───────────────────────────────────────────────────────────

func (s *PgStore) CreateSubscription(ctx context.Context, sub *whout.Subscription, e whout.LogEntry) (*whout.Subscription, error) {
	var out *whout.Subscription
	err := s.withRLS(ctx, sub.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO webhook_subscriptions (subscription_id, tenant_id, legal_entity_id, name, destination_url, event_types,
				allowed_fields, api_version, signing_key_path, destination_region, max_attempts, status, version,
				config_fingerprint, created_by, last_changed_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'DRAFT',1,$12,$13,$13)
			RETURNING `+subCols,
			sub.SubscriptionID, sub.TenantID, sub.LegalEntityID, sub.Name, sub.DestinationURL, sub.EventTypes,
			sub.AllowedFields, sub.APIVersion, sub.SigningKeyPath, sub.DestinationRegion, sub.MaxAttempts,
			sub.ConfigFingerprint, sub.CreatedBy)
		var err error
		if out, err = scanSub(row); err != nil {
			return constraintErr(err)
		}
		e.TenantID, e.SubscriptionID, e.Version, e.ToStatus, e.Fingerprint = out.TenantID, out.SubscriptionID, out.Version, out.Status, out.ConfigFingerprint
		return insertLog(ctx, tx, e)
	})
	return out, err
}

func (s *PgStore) GetSubscription(ctx context.Context, tenantID, id string) (*whout.Subscription, error) {
	var out *whout.Subscription
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanSub(tx.QueryRow(ctx, `SELECT `+subCols+` FROM webhook_subscriptions WHERE subscription_id = $1 AND tenant_id = $2`, id, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || pgCode(err) == "22P02" { // not found, or not a UUID
		return nil, whout.ErrNotFound
	}
	return out, err
}

func (s *PgStore) ListSubscriptions(ctx context.Context, tenantID string, status whout.Status, limit int) ([]whout.Subscription, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []whout.Subscription
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+subCols+` FROM webhook_subscriptions
			WHERE tenant_id = $1 AND ($2 = '' OR status = $2) ORDER BY created_at DESC LIMIT $3`, tenantID, string(status), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sub, err := scanSub(rows)
			if err != nil {
				return err
			}
			out = append(out, *sub)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ApplyChange(ctx context.Context, ch whout.Change) (*whout.Subscription, error) {
	n := ch.Sub
	var out *whout.Subscription
	err := s.withRLS(ctx, ch.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE webhook_subscriptions SET
				name = $3, destination_url = $4, event_types = $5, allowed_fields = $6, api_version = $7,
				signing_key_path = $8, previous_signing_key_path = $9, previous_key_valid_until = $10,
				destination_region = $11, max_attempts = $12, status = $13, status_reason = $14,
				version = version + 1, config_fingerprint = $15, approved_fingerprint = $16,
				last_changed_by = $17, approved_by = $18, approved_at = $19,
				consecutive_failures = CASE WHEN $21 THEN 0 ELSE consecutive_failures END,
				consecutive_dead_letters = CASE WHEN $21 THEN 0 ELSE consecutive_dead_letters END,
				breaker_open_until = CASE WHEN $21 THEN NULL ELSE breaker_open_until END
			WHERE subscription_id = $1 AND tenant_id = $2 AND version = $20
			RETURNING `+subCols,
			n.SubscriptionID, ch.TenantID, n.Name, n.DestinationURL, n.EventTypes, n.AllowedFields, n.APIVersion,
			n.SigningKeyPath, nullStr(n.PreviousSigningKeyPath), n.PreviousKeyValidUntil,
			n.DestinationRegion, n.MaxAttempts, string(n.Status), n.StatusReason,
			n.ConfigFingerprint, nullStr(n.ApprovedFingerprint),
			n.LastChangedBy, nullStr(n.ApprovedBy), n.ApprovedAt, ch.ExpectedVersion, ch.ResetHealth)
		var err error
		out, err = scanSub(row)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_subscriptions WHERE subscription_id = $1 AND tenant_id = $2)`,
				n.SubscriptionID, ch.TenantID).Scan(&exists)
			if !exists {
				return whout.ErrNotFound
			}
			return whout.ErrVersionConflict
		}
		if err != nil {
			return constraintErr(err)
		}
		ch.Log.TenantID, ch.Log.SubscriptionID, ch.Log.Version, ch.Log.ToStatus, ch.Log.Fingerprint = out.TenantID, out.SubscriptionID, out.Version, out.Status, out.ConfigFingerprint
		if err := insertLog(ctx, tx, ch.Log); err != nil {
			return err
		}
		if ch.DeadLetterPending {
			if _, err := tx.Exec(ctx, `
				UPDATE webhook_deliveries SET status = 'DEAD_LETTERED', dead_letter_reason = $3, dead_lettered_at = now(), locked_until = NULL
				WHERE subscription_id = $1 AND tenant_id = $2 AND status IN ('PENDING', 'RETRYING', 'SENT')`,
				out.SubscriptionID, ch.TenantID, ch.DeadLetterReason); err != nil {
				return err
			}
		}
		if ch.Event != nil {
			return enqueue(ctx, tx, ch.TenantID, *ch.Event)
		}
		return nil
	})
	return out, err
}

func (s *PgStore) ListLog(ctx context.Context, tenantID, subscriptionID string, limit int) ([]whout.LogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []whout.LogEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT log_id, tenant_id, subscription_id::text, kind, actor, COALESCE(approver, ''), COALESCE(approval_ref, ''),
				COALESCE(from_status, ''), COALESCE(to_status, ''), version, COALESCE(fingerprint, ''),
				COALESCE(delivery_id::text, ''), reason, correlation_id, created_at
			FROM webhook_subscription_log WHERE tenant_id = $1 AND subscription_id = $2 ORDER BY log_id LIMIT $3`,
			tenantID, subscriptionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e whout.LogEntry
			var kind, from, to string
			if err := rows.Scan(&e.LogID, &e.TenantID, &e.SubscriptionID, &kind, &e.Actor, &e.Approver, &e.ApprovalRef,
				&from, &to, &e.Version, &e.Fingerprint, &e.DeliveryID, &e.Reason, &e.CorrelationID, &e.CreatedAt); err != nil {
				return err
			}
			e.Kind, e.FromStatus, e.ToStatus = whout.LogKind(kind), whout.Status(from), whout.Status(to)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ── fan-out (runs inside the inbox transaction) ─────────────────────────────

func (s *PgStore) FanOut(ctx context.Context, tx pgx.Tx, ev whout.SourceEvent) (whout.FanOutResult, error) {
	var res whout.FanOutResult
	if whout.IsOwnEvent(ev.EventType) {
		return res, nil // an event about a delivery is never itself delivered
	}
	rows, err := tx.Query(ctx, `SELECT `+subCols+` FROM webhook_subscriptions WHERE tenant_id = $1 AND status = 'ACTIVE'`, ev.TenantID)
	if err != nil {
		return res, err
	}
	var subs []*whout.Subscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			rows.Close()
			return res, err
		}
		subs = append(subs, sub)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, sub := range subs {
		if !whout.MatchEventType(sub.EventTypes, ev.EventType) {
			continue
		}
		// A subscription receives the events that occurred after it was activated, never
		// the history: a consumer that starts at the beginning of a topic (a new group, a
		// replay) must not flood a destination with events from before it was approved.
		if sub.ApprovedAt != nil && ev.OccurredAt.Before(*sub.ApprovedAt) {
			res.BeforeActivation++
			continue
		}
		res.Matched++
		body, err := whout.Minimize(ev, sub.AllowedFields, sub.APIVersion)
		if err != nil {
			res.Blocked++ // never fall back to a fuller body: no delivery is better than a leak
			continue
		}
		sum := sha256.Sum256(body)
		tag, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (delivery_id, tenant_id, subscription_id, event_id, event_type, event_occurred_at,
				body, body_hash, scope_fingerprint, status, next_attempt_at, correlation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'PENDING', now(), $10)
			ON CONFLICT (subscription_id, event_id) DO NOTHING`,
			whout.DeliveryID(sub.SubscriptionID, ev.EventID), ev.TenantID, sub.SubscriptionID, ev.EventID, ev.EventType,
			ev.OccurredAt, body, hex.EncodeToString(sum[:]), whout.ScopeFingerprint(sub.AllowedFields), ev.EventID)
		if err != nil {
			return res, err
		}
		if tag.RowsAffected() == 0 {
			res.Duplicates++
		} else {
			res.Created++
		}
	}
	return res, nil
}

// ── dispatch ────────────────────────────────────────────────────────────────

func (s *PgStore) ClaimDue(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]whout.Claim, error) {
	var out []whout.Claim
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.webhook_dispatch', 'true', true)"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		WITH due AS (
			SELECT delivery_id FROM webhook_deliveries
			WHERE (status IN ('PENDING', 'RETRYING') AND next_attempt_at <= $1 AND (locked_until IS NULL OR locked_until < $1))
			   OR (status = 'SENT' AND locked_until < $1)
			ORDER BY next_attempt_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE webhook_deliveries d SET locked_until = $1::timestamptz + make_interval(secs => $3)
		FROM due WHERE d.delivery_id = due.delivery_id
		RETURNING d.tenant_id, d.delivery_id::text`, now, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c whout.Claim
		if err := rows.Scan(&c.TenantID, &c.DeliveryID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func (s *PgStore) LoadForSend(ctx context.Context, tenantID, deliveryID string) (*whout.Delivery, *whout.Subscription, error) {
	var d *whout.Delivery
	var sub *whout.Subscription
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		if d, err = scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE delivery_id = $1 AND tenant_id = $2`, deliveryID, tenantID)); err != nil {
			return err
		}
		sub, err = scanSub(tx.QueryRow(ctx, `SELECT `+subCols+` FROM webhook_subscriptions WHERE subscription_id = $1 AND tenant_id = $2`, d.SubscriptionID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, whout.ErrDeliveryNotFound
	}
	return d, sub, err
}

func (s *PgStore) BeginAttempt(ctx context.Context, tenantID string, d *whout.Delivery, host string, now time.Time, lease time.Duration) (int, error) {
	var no int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var count int
		if err := tx.QueryRow(ctx, `SELECT status, attempt_count FROM webhook_deliveries WHERE delivery_id = $1 AND tenant_id = $2 FOR UPDATE`,
			d.DeliveryID, tenantID).Scan(&status, &count); err != nil {
			return err
		}
		if status != "PENDING" && status != "RETRYING" && status != "SENT" {
			return fmt.Errorf("%w: delivery is %s", whout.ErrInvalidState, status)
		}
		// An attempt left IN_FLIGHT by a worker that died mid-send has an outcome nobody
		// knows. Record it as such; it already counted against the budget.
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_delivery_attempts SET outcome = 'UNKNOWN', finished_at = $3, error_class = 'lease_expired'
			WHERE delivery_id = $1 AND tenant_id = $2 AND outcome = 'IN_FLIGHT'`, d.DeliveryID, tenantID, now); err != nil {
			return err
		}
		no = count + 1
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'SENT', attempt_count = $3, last_attempt_at = $4,
				locked_until = $4::timestamptz + make_interval(secs => $5)
			WHERE delivery_id = $1 AND tenant_id = $2`, d.DeliveryID, tenantID, no, now, lease.Seconds()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO webhook_delivery_attempts (tenant_id, delivery_id, subscription_id, attempt_no, started_at, outcome,
				destination_host, payload_hash)
			VALUES ($1, $2, $3, $4, $5, 'IN_FLIGHT', $6, $7)`,
			tenantID, d.DeliveryID, d.SubscriptionID, no, now, host, d.BodyHash)
		return err
	})
	return no, err
}

func (s *PgStore) FinishAttempt(ctx context.Context, in whout.FinishInput) error {
	return s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		d, err := scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE delivery_id = $1 AND tenant_id = $2 FOR UPDATE`, in.DeliveryID, in.TenantID))
		if err != nil {
			return err
		}
		sub, err := scanSub(tx.QueryRow(ctx, `SELECT `+subCols+` FROM webhook_subscriptions WHERE subscription_id = $1 AND tenant_id = $2 FOR UPDATE`, d.SubscriptionID, in.TenantID))
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE webhook_delivery_attempts SET outcome = $4, finished_at = $5, response_code = $6, response_body_hash = $7,
				response_body_bytes = $8, error_class = $9, signature_timestamp = $10, signature_key_paths = $11
			WHERE delivery_id = $1 AND tenant_id = $2 AND attempt_no = $3 AND outcome = 'IN_FLIGHT'`,
			in.DeliveryID, in.TenantID, in.AttemptNo, string(in.Outcome), in.Now, in.ResponseCode, nullStr(in.ResponseBodyHash),
			in.ResponseBodyBytes, nullStr(in.ErrorClass), in.SignatureTimestamp, nonNilStrings(in.SignatureKeyPaths))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: attempt %d is not in flight", whout.ErrInvalidState, in.AttemptNo)
		}

		// The subscription was revoked, or the delivery dead-lettered, while this attempt
		// was in flight. The attempt's evidence is recorded (above) and nothing else
		// moves: a revoked subscription and a dead letter are final until a replay.
		if sub.Status == whout.StatusRevoked || d.Status == whout.DeliveryDeadLettered {
			return nil
		}

		var succeededAt, deadAt *time.Time
		switch in.NewStatus {
		case whout.DeliverySucceeded:
			succeededAt = &in.Now
		case whout.DeliveryDeadLettered:
			deadAt = &in.Now
		}
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET status = $3, next_attempt_at = $4, locked_until = NULL, last_response_code = $5,
				last_error_class = $6, dead_letter_reason = $7, dead_lettered_at = $8, succeeded_at = $9
			WHERE delivery_id = $1 AND tenant_id = $2`,
			in.DeliveryID, in.TenantID, string(in.NewStatus), in.NextAttemptAt, in.ResponseCode, nullStr(in.ErrorClass),
			nullStr(in.DeadLetterReason), deadAt, succeededAt); err != nil {
			return err
		}

		// destination health
		var disable bool
		switch {
		case in.Success:
			if _, err := tx.Exec(ctx, `
				UPDATE webhook_subscriptions SET consecutive_failures = 0, consecutive_dead_letters = 0,
					breaker_open_until = NULL, last_success_at = $3 WHERE subscription_id = $1 AND tenant_id = $2`,
				sub.SubscriptionID, in.TenantID, in.Now); err != nil {
				return err
			}
		case in.Failure:
			failures := sub.ConsecutiveFailures + 1
			dead := sub.ConsecutiveDeadLetters
			if in.NewStatus == whout.DeliveryDeadLettered {
				dead++
			}
			var openUntil *time.Time
			if in.BreakerThreshold > 0 && failures >= in.BreakerThreshold {
				t := in.Now.Add(in.BreakerCooldown)
				openUntil = &t
			}
			disable = sub.Status == whout.StatusActive && in.DisableAfterDeadLetters > 0 && dead >= in.DisableAfterDeadLetters
			if _, err := tx.Exec(ctx, `
				UPDATE webhook_subscriptions SET consecutive_failures = $3, consecutive_dead_letters = $4,
					breaker_open_until = $5, last_failure_at = $6 WHERE subscription_id = $1 AND tenant_id = $2`,
				sub.SubscriptionID, in.TenantID, failures, dead, openUntil, in.Now); err != nil {
				return err
			}
		}

		// events
		if in.Failure || in.ControlFailure {
			var ev events.Outbound
			var err error
			if in.ControlFailure {
				ev, err = whout.ControlFailedEvent(in.TenantID, sub.LegalEntityID, d, in.DeadLetterReason)
			} else {
				ev, err = whout.FailedEvent(in.TenantID, sub.LegalEntityID, d, in.AttemptNo, in.Outcome, in.ErrorClass)
			}
			if err != nil {
				return err
			}
			if err := enqueue(ctx, tx, in.TenantID, ev); err != nil {
				return err
			}
		}
		if in.NewStatus == whout.DeliveryDeadLettered {
			ev, err := whout.DeadLetteredEvent(in.TenantID, sub.LegalEntityID, d, in.AttemptNo, in.Outcome, in.ErrorClass, in.DeadLetterReason)
			if err != nil {
				return err
			}
			if err := enqueue(ctx, tx, in.TenantID, ev); err != nil {
				return err
			}
		}
		if disable {
			// The platform stops a destination that keeps dead-lettering. Approval and
			// configuration are untouched, so an operator can resume it once the
			// destination is fixed.
			next := *sub
			next.Status, next.StatusReason = whout.StatusDisabled, "disabled after repeated dead-lettered deliveries"
			row := tx.QueryRow(ctx, `
				UPDATE webhook_subscriptions SET status = 'DISABLED', status_reason = $3, version = version + 1
				WHERE subscription_id = $1 AND tenant_id = $2 AND status = 'ACTIVE' RETURNING `+subCols,
				sub.SubscriptionID, in.TenantID, next.StatusReason)
			upd, err := scanSub(row)
			if err != nil {
				return constraintErr(err)
			}
			if err := insertLog(ctx, tx, whout.LogEntry{
				TenantID: in.TenantID, SubscriptionID: upd.SubscriptionID, Kind: whout.LogDisabled, Actor: "system:webhook-dispatcher",
				FromStatus: whout.StatusActive, ToStatus: whout.StatusDisabled, Version: upd.Version, Fingerprint: upd.ConfigFingerprint,
				Reason: next.StatusReason,
			}); err != nil {
				return err
			}
			ev, err := whout.DisabledEvent(upd, "system:webhook-dispatcher", d.CorrelationID, next.StatusReason)
			if err != nil {
				return err
			}
			return enqueue(ctx, tx, in.TenantID, ev)
		}
		return nil
	})
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func (s *PgStore) Defer(ctx context.Context, tenantID, deliveryID string, until time.Time) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET next_attempt_at = $3, locked_until = NULL
			WHERE delivery_id = $1 AND tenant_id = $2 AND status IN ('PENDING', 'RETRYING')`, deliveryID, tenantID, until)
		return err
	})
}

// ── queries ─────────────────────────────────────────────────────────────────

func (s *PgStore) GetDelivery(ctx context.Context, tenantID, deliveryID string) (*whout.Delivery, error) {
	var d *whout.Delivery
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		d, err = scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE delivery_id = $1 AND tenant_id = $2`, deliveryID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || pgCode(err) == "22P02" {
		return nil, whout.ErrDeliveryNotFound
	}
	return d, err
}

func (s *PgStore) ListDeliveries(ctx context.Context, tenantID, subscriptionID string, status whout.DeliveryStatus, limit int) ([]whout.Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []whout.Delivery
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
			WHERE tenant_id = $1 AND ($2 = '' OR subscription_id::text = $2) AND ($3 = '' OR status = $3)
			ORDER BY created_at DESC LIMIT $4`, tenantID, subscriptionID, string(status), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelivery(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ListDeliveryAttempts(ctx context.Context, tenantID, deliveryID string) ([]whout.Attempt, error) {
	var out []whout.Attempt
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT attempt_id, delivery_id::text, subscription_id::text, attempt_no, started_at, finished_at, outcome, response_code,
				COALESCE(response_body_hash, ''), response_body_bytes, COALESCE(error_class, ''), destination_host, payload_hash,
				signature_profile, signature_timestamp, signature_key_paths
			FROM webhook_delivery_attempts WHERE tenant_id = $1 AND delivery_id::text = $2 ORDER BY attempt_no`, tenantID, deliveryID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a whout.Attempt
			var oc string
			if err := rows.Scan(&a.AttemptID, &a.DeliveryID, &a.SubscriptionID, &a.AttemptNo, &a.StartedAt, &a.FinishedAt, &oc,
				&a.ResponseCode, &a.ResponseBodyHash, &a.ResponseBodyBytes, &a.ErrorClass, &a.DestinationHost, &a.PayloadHash,
				&a.SignatureProfile, &a.SignatureTimestamp, &a.SignatureKeyPaths); err != nil {
				return err
			}
			a.Outcome = whout.AttemptOutcome(oc)
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) Health(ctx context.Context, tenantID, subscriptionID string, now time.Time) (*whout.Health, error) {
	var h *whout.Health
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		sub, err := scanSub(tx.QueryRow(ctx, `SELECT `+subCols+` FROM webhook_subscriptions WHERE subscription_id = $1 AND tenant_id = $2`, subscriptionID, tenantID))
		if err != nil {
			return err
		}
		h = &whout.Health{
			SubscriptionID: sub.SubscriptionID, Status: sub.Status, BreakerOpenUntil: sub.BreakerOpenUntil,
			BreakerOpen:         sub.BreakerOpenUntil != nil && sub.BreakerOpenUntil.After(now),
			ConsecutiveFailures: sub.ConsecutiveFailures, ConsecutiveDeadLetters: sub.ConsecutiveDeadLetters,
			LastSuccessAt: sub.LastSuccessAt, LastFailureAt: sub.LastFailureAt,
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status IN ('PENDING', 'SENT')), count(*) FILTER (WHERE status = 'RETRYING'),
				count(*) FILTER (WHERE status = 'DEAD_LETTERED'), count(*) FILTER (WHERE status = 'SUCCEEDED')
			FROM webhook_deliveries WHERE subscription_id = $1 AND tenant_id = $2`, subscriptionID, tenantID).
			Scan(&h.Pending, &h.Retrying, &h.DeadLettered, &h.Succeeded)
	})
	if errors.Is(err, pgx.ErrNoRows) || pgCode(err) == "22P02" {
		return nil, whout.ErrNotFound
	}
	return h, err
}

func (s *PgStore) Replay(ctx context.Context, tenantID, deliveryID string, e whout.LogEntry, ev *events.Outbound, now time.Time) (*whout.Delivery, error) {
	var out *whout.Delivery
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE delivery_id = $1 AND tenant_id = $2 FOR UPDATE`, deliveryID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return whout.ErrDeliveryNotFound
		}
		if err != nil {
			return err
		}
		if d.Status != whout.DeliveryDeadLettered {
			return whout.ErrReplayNotAllowed
		}
		// The same delivery identity goes back on the queue with a fresh retry budget;
		// attempt history and the attempt counter are kept.
		out, err = scanDelivery(tx.QueryRow(ctx, `
			UPDATE webhook_deliveries SET status = 'PENDING', next_attempt_at = $3, locked_until = NULL, replay_count = replay_count + 1,
				budget_base = attempt_count, dead_letter_reason = NULL, dead_lettered_at = NULL
			WHERE delivery_id = $1 AND tenant_id = $2 RETURNING `+deliveryCols, deliveryID, tenantID, now))
		if err != nil {
			return constraintErr(err)
		}
		e.TenantID, e.SubscriptionID, e.DeliveryID = tenantID, d.SubscriptionID, d.DeliveryID
		if err := insertLog(ctx, tx, e); err != nil {
			return constraintErr(err)
		}
		if ev != nil {
			return enqueue(ctx, tx, tenantID, *ev)
		}
		return nil
	})
	return out, err
}

func (s *PgStore) HasMatchingActive(ctx context.Context, tenantID, eventType string) (bool, error) {
	if whout.IsOwnEvent(eventType) {
		return false, nil
	}
	var filters [][]string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT event_types FROM webhook_subscriptions WHERE tenant_id = $1 AND status = 'ACTIVE'`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f []string
			if err := rows.Scan(&f); err != nil {
				return err
			}
			filters = append(filters, f)
		}
		return rows.Err()
	})
	if err != nil {
		return false, err
	}
	for _, f := range filters {
		if whout.MatchEventType(f, eventType) {
			return true, nil
		}
	}
	return false, nil
}
