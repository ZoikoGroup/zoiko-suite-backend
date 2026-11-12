package store

// Outbox relay claims (J-15) and parked-event recovery (J-16).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// TryLockOutboxRelay takes the relay's transaction-scoped advisory lock. It
// must run inside the relay's unit of work; it returns false when another
// replica's relay holds it, and that replica publishes this tick instead.
//
// The claim itself is FOR UPDATE SKIP LOCKED (ClaimOutboxEvents), so two
// relays can never publish the same row. The lock adds ordering: with one
// relay at a time, events of one aggregate leave in the order they were
// written, which disjoint claims by two relays would not guarantee.
func (s *PgStore) TryLockOutboxRelay(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db(ctx).QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('jurisdiction-rules-svc:outbox-relay'))`).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("%w: relay lock: %v", domain.ErrStoreUnavailable, err)
	}
	return ok, nil
}

// ClaimOutboxEvents locks up to limit unpublished, unparked events for the
// caller's transaction (FOR UPDATE SKIP LOCKED): rows another relay holds are
// skipped, never published twice. Outside a transaction the locks last only
// for the statement, so the relay calls it inside a unit of work.
func (s *PgStore) ClaimOutboxEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT outbox_id, event_type, event_payload, topic, partition_key, created_at, publish_attempts
		FROM event_outbox
		WHERE published_at IS NULL AND publish_attempts < $2
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit, MaxOutboxAttempts)
	if err != nil {
		s.log.Error("pg ClaimOutboxEvents failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var out []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		if err := rows.Scan(&e.OutboxID, &e.EventType, &e.EventPayload, &e.Topic, &e.PartitionKey, &e.CreatedAt, &e.PublishAttempts); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ParkedOutboxEvent is an event the relay stopped retrying.
type ParkedOutboxEvent struct {
	OutboxID        string           `json:"outbox_id"`
	EventType       string           `json:"event_type"`
	Topic           string           `json:"topic"`
	PartitionKey    *string          `json:"partition_key,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	PublishAttempts int              `json:"publish_attempts"`
	LastError       *string          `json:"last_error,omitempty"`
	Requeues        []*OutboxRequeue `json:"requeues"`
}

// OutboxRequeue is one recorded requeue of a parked event (append-only, 000033).
type OutboxRequeue struct {
	RequeueID      string    `json:"requeue_id"`
	OutboxID       string    `json:"outbox_id"`
	EventType      string    `json:"event_type"`
	AttemptsBefore int       `json:"attempts_before"`
	LastError      *string   `json:"last_error,omitempty"`
	Reason         string    `json:"reason"`
	EvidenceRef    *string   `json:"evidence_ref,omitempty"`
	RequestedBy    string    `json:"requested_by"`
	RequestedAt    time.Time `json:"requested_at"`
}

const requeueCols = `requeue_id::text, outbox_id::text, event_type, attempts_before, last_error, reason, evidence_ref, requested_by, requested_at`

func scanRequeue(r pgx.Row) (*OutboxRequeue, error) {
	var x OutboxRequeue
	err := r.Scan(&x.RequeueID, &x.OutboxID, &x.EventType, &x.AttemptsBefore, &x.LastError, &x.Reason, &x.EvidenceRef, &x.RequestedBy, &x.RequestedAt)
	return &x, err
}

// ListParkedOutboxEvents lists parked events, oldest first, with the requeues
// each has already had. It reads limit+1 rows so the caller can tell whether
// another page exists.
func (s *PgStore) ListParkedOutboxEvents(ctx context.Context, limit, offset int) ([]*ParkedOutboxEvent, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT outbox_id::text, event_type, topic, partition_key, created_at, publish_attempts, last_error
		FROM event_outbox
		WHERE published_at IS NULL AND publish_attempts >= $1
		ORDER BY created_at ASC, outbox_id ASC
		LIMIT $2 OFFSET $3`, MaxOutboxAttempts, limit, offset)
	if err != nil {
		return nil, s.registryFail("ListParkedOutboxEvents", err, nil)
	}
	out := []*ParkedOutboxEvent{}
	for rows.Next() {
		var e ParkedOutboxEvent
		if err := rows.Scan(&e.OutboxID, &e.EventType, &e.Topic, &e.PartitionKey, &e.CreatedAt, &e.PublishAttempts, &e.LastError); err != nil {
			rows.Close()
			return nil, s.registryFail("ListParkedOutboxEvents scan", err, nil)
		}
		out = append(out, &e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, s.registryFail("ListParkedOutboxEvents rows", err, nil)
	}
	for _, e := range out {
		if e.Requeues, err = s.outboxRequeues(ctx, e.OutboxID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *PgStore) outboxRequeues(ctx context.Context, outboxID string) ([]*OutboxRequeue, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+requeueCols+` FROM outbox_requeues WHERE outbox_id::text=$1 ORDER BY requested_at, requeue_id`, outboxID)
	if err != nil {
		return nil, s.registryFail("outboxRequeues", err, nil)
	}
	defer rows.Close()
	out := []*OutboxRequeue{}
	for rows.Next() {
		x, err := scanRequeue(rows)
		if err != nil {
			return nil, s.registryFail("outboxRequeues scan", err, nil)
		}
		out = append(out, x)
	}
	return out, s.registryFail("outboxRequeues rows", rows.Err(), nil)
}

// Outbox recovery errors.
var (
	ErrOutboxEventNotFound  = errors.New("outbox event not found")
	ErrOutboxEventNotParked = errors.New("outbox event is not parked: it is published, or still being retried")
)

// RequeueOutboxEvent puts a parked event back in the relay's queue: its
// attempt count and last error are reset, and the requeue is recorded with
// who asked, why and on what evidence. Only a parked event can be requeued;
// the row is locked, so two operators cannot requeue it twice.
func (s *PgStore) RequeueOutboxEvent(ctx context.Context, outboxID, reason, evidenceRef, actor string) (*OutboxRequeue, error) {
	tx, err := s.begin(ctx, "RequeueOutboxEvent")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eventType string
	var attempts int
	var lastError *string
	var published *time.Time
	err = tx.QueryRow(ctx, `SELECT event_type, publish_attempts, last_error, published_at FROM event_outbox WHERE outbox_id::text=$1 FOR UPDATE`, outboxID).
		Scan(&eventType, &attempts, &lastError, &published)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidTextRepresentation(err) {
			return nil, ErrOutboxEventNotFound
		}
		return nil, s.registryFail("RequeueOutboxEvent read", err, nil)
	}
	if published != nil || attempts < MaxOutboxAttempts {
		return nil, ErrOutboxEventNotParked
	}
	x, err := scanRequeue(tx.QueryRow(ctx, `INSERT INTO outbox_requeues (outbox_id, event_type, attempts_before, last_error, reason, evidence_ref, requested_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7) RETURNING `+requeueCols,
		outboxID, eventType, attempts, lastError, strings.TrimSpace(reason), nullIfEmpty(evidenceRef), actor))
	if err != nil {
		return nil, s.registryFail("RequeueOutboxEvent record", err, nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE event_outbox SET publish_attempts = 0, last_error = NULL WHERE outbox_id::text=$1`, outboxID); err != nil {
		return nil, s.registryFail("RequeueOutboxEvent reset", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("RequeueOutboxEvent commit", err, nil)
	}
	return x, nil
}
