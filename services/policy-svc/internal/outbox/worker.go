// Package outbox publishes the events the store wrote to the outbox table in
// the same transaction as each state change (GCP §2 invariant #10, V-001
// §20.2): at-least-once delivery after commit, with a stable event_id per row
// so consumers can deduplicate.
package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/events"
)

const (
	pollInterval = 500 * time.Millisecond
	batchSize    = 100
	// lease must outlast one WriteMessages call for a full batch.
	lease      = time.Minute
	maxBackoff = 5 * time.Minute
)

// Writer is the one method the relay needs from *kafka.Writer.
type Writer interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type row struct {
	id               int64
	eventID          string
	eventType        string
	eventVersion     string
	schemaVersion    string
	sourceService    string
	tenantID         *string
	legalEntityID    *string
	actorID          *string
	correlationID    *string
	causationID      *string
	jurisdiction     *string
	idempotencyKey   string
	aggregateID      *string
	aggregateVersion *int
	payload          []byte
	createdAt        time.Time
	attempts         int
}

// Worker polls the outbox table and publishes events to Kafka.
type Worker struct {
	pool     *pgxpool.Pool
	producer Writer
	log      *zap.Logger
	// residency is the residency region stamped on every event this
	// deployment publishes (EVENT_RESIDENCY_REGION).
	residency string
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewWorker constructs an outbox worker. The topic is the Writer's own: a
// kafka.Message that also names a topic is refused by kafka-go when the
// Writer has one, which is why the previous relay never published anything.
func NewWorker(pool *pgxpool.Pool, producer Writer, log *zap.Logger) *Worker {
	return &Worker{pool: pool, producer: producer, log: log, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// WithResidency sets the residency region stamped on published events.
func (w *Worker) WithResidency(region string) *Worker {
	w.residency = region
	return w
}

// Start begins the polling loop in a background goroutine.
func (w *Worker) Start() {
	go w.run()
}

// Stop signals the worker to stop and waits for an in-flight batch.
func (w *Worker) Stop() {
	close(w.stopCh)
	<-w.doneCh
}

func (w *Worker) run() {
	defer close(w.doneCh)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), lease)
			w.RunOnce(ctx)
			cancel()
		}
	}
}

func backoff(attempts int) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// RunOnce claims and publishes one batch, returning how many were published.
func (w *Worker) RunOnce(ctx context.Context) int {
	now := time.Now().UTC()
	// The lease (next_attempt_at pushed forward) keeps a second instance off
	// these rows while this one publishes them.
	rows, err := w.pool.Query(ctx, `
		UPDATE outbox SET next_attempt_at = $3, attempts = attempts + 1
		WHERE outbox_id IN (
			SELECT outbox_id FROM outbox
			WHERE published_at IS NULL AND next_attempt_at <= $1
			ORDER BY outbox_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING outbox_id, event_id::text, event_type, event_version, schema_version, source_service,
		          tenant_id::text, legal_entity_id::text, actor_id, correlation_id, causation_id,
		          idempotency_key, aggregate_id, aggregate_version, payload, created_at, attempts,
		          jurisdiction_context;`,
		now, batchSize, now.Add(lease))
	if err != nil {
		w.log.Error("outbox: claim failed", zap.Error(err))
		return 0
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.eventID, &r.eventType, &r.eventVersion, &r.schemaVersion, &r.sourceService,
			&r.tenantID, &r.legalEntityID, &r.actorID, &r.correlationID, &r.causationID,
			&r.idempotencyKey, &r.aggregateID, &r.aggregateVersion, &r.payload, &r.createdAt, &r.attempts,
			&r.jurisdiction); err != nil {
			rows.Close()
			w.log.Error("outbox: scan failed", zap.Error(err))
			return 0
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		w.log.Error("outbox: rows error", zap.Error(err))
		return 0
	}
	if len(batch) == 0 {
		return 0
	}

	// Claim order is outbox_id order, and one WriteMessages call keeps it, so
	// events of one aggregate (one message key) stay in order.
	failed := map[int64]string{}
	var sendable []row
	var msgs []kafka.Message
	for _, r := range batch {
		ev := r.toEvent()
		ev.ResidencyRegion = w.residency
		m, err := Message(ev)
		if err != nil {
			failed[r.id] = "encode: " + err.Error()
			continue
		}
		sendable = append(sendable, r)
		msgs = append(msgs, m)
	}

	var published []int64
	if len(msgs) > 0 {
		err = w.producer.WriteMessages(ctx, msgs...)
		var writeErrs kafka.WriteErrors
		for i, r := range sendable {
			switch {
			case err == nil:
				published = append(published, r.id)
			case errors.As(err, &writeErrs) && len(writeErrs) == len(sendable) && writeErrs[i] == nil:
				published = append(published, r.id)
			default:
				failed[r.id] = fmt.Sprint(err)
			}
		}
	}

	if len(published) > 0 {
		if _, err := w.pool.Exec(ctx, `UPDATE outbox SET published_at = $2, last_error = NULL WHERE outbox_id = ANY($1);`, published, time.Now().UTC()); err != nil {
			// Re-published after the lease: consumers dedupe by event_id.
			w.log.Error("outbox: mark published failed", zap.Error(err))
		}
	}
	for _, r := range batch {
		msg, ok := failed[r.id]
		if !ok {
			continue
		}
		next := time.Now().UTC().Add(backoff(r.attempts))
		if _, uerr := w.pool.Exec(ctx, `UPDATE outbox SET last_error = $2, next_attempt_at = $3 WHERE outbox_id = $1;`, r.id, msg, next); uerr != nil {
			w.log.Error("outbox: mark retry failed", zap.Error(uerr))
		}
	}
	if len(failed) > 0 {
		w.log.Warn("outbox: publish failed; will retry", zap.Int("failed", len(failed)))
	}
	return len(published)
}

// Event is one outbox row in envelope form.
type Event struct {
	EventID          string
	EventType        string
	EventVersion     string
	SchemaVersion    string
	SourceService    string
	TenantID         string
	LegalEntityID    string
	ActorID          string
	CorrelationID    string
	CausationID      string
	IdempotencyKey   string
	AggregateID      string
	AggregateVersion int
	Payload          []byte
	OccurredAt       time.Time

	JurisdictionContext string
	ResidencyRegion     string
}

func (r row) toEvent() Event {
	s := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	agg := 0
	if r.aggregateVersion != nil {
		agg = *r.aggregateVersion
	}
	return Event{
		EventID: r.eventID, EventType: r.eventType, EventVersion: r.eventVersion, SchemaVersion: r.schemaVersion,
		SourceService: r.sourceService, TenantID: s(r.tenantID), LegalEntityID: s(r.legalEntityID),
		ActorID: s(r.actorID), CorrelationID: s(r.correlationID), CausationID: s(r.causationID),
		IdempotencyKey: r.idempotencyKey, AggregateID: s(r.aggregateID), AggregateVersion: agg,
		Payload: r.payload, OccurredAt: r.createdAt, JurisdictionContext: s(r.jurisdiction),
	}
}

// Message wraps e in the platform envelope (Doc 03 §19, Event Catalogue §4).
// event_id is the outbox row's own, so every redelivery carries the same id;
// payload_hash is over the exact payload bytes stored and sent.
func Message(e Event) (kafka.Message, error) {
	causation := e.CausationID
	if causation == "" {
		causation = e.CorrelationID
	}
	env := events.Envelope{
		EventID:             e.EventID,
		EventType:           e.EventType,
		EventVersion:        e.EventVersion,
		EmittedAt:           e.OccurredAt.UTC(),
		SchemaVersion:       e.SchemaVersion,
		SourceService:       e.SourceService,
		TenantID:            e.TenantID,
		LegalEntityID:       e.LegalEntityID,
		ActorID:             e.ActorID,
		CorrelationID:       e.CorrelationID,
		CausationID:         causation,
		JurisdictionContext: e.JurisdictionContext,
		ResidencyRegion:     e.ResidencyRegion,
		Classification:      "governance",
		PayloadHash:         fmt.Sprintf("sha256:%x", sha256.Sum256(e.Payload)),
		AggregateVersion:    strconv.Itoa(e.AggregateVersion),
		PublishedAt:         time.Now().UTC(),
		Payload:             json.RawMessage(e.Payload),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return kafka.Message{}, err
	}
	key := e.AggregateID
	if key == "" {
		key = e.EventID
	}
	return kafka.Message{
		Key:   []byte(key),
		Value: data,
		Headers: []kafka.Header{
			{Key: "event_id", Value: []byte(e.EventID)},
			{Key: "event_type", Value: []byte(e.EventType)},
			{Key: "Idempotency-Key", Value: []byte(e.IdempotencyKey)},
		},
	}, nil
}
