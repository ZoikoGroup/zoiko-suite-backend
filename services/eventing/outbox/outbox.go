// Package outbox is the shared transactional outbox and dispatcher required by
// ZS-EVENT-001 §6 ("Every authoritative producer of a material event must make
// the state change and corresponding outbox record part of one local atomic
// transaction").
//
// THE PROBLEM IT SOLVES. A service that commits to Postgres and then writes to
// Kafka loses the event whenever it dies, or Kafka is unreachable, between
// the two. Enqueue writes the event in the producer's own transaction, so the
// fact and the event commit together or not at all; the Relay then delivers
// what committed, retrying until the broker takes it.
//
// WHAT THE RELAY GUARANTEES, AND WHAT IT DOES NOT. Delivery is at-least-once
// (§7). A row is leased while it is being published (publish_state 'claimed'),
// so concurrent relays on several replicas divide the backlog instead of
// publishing the same rows twice; a relay that dies mid-publish leaves a lease
// that expires and the row is delivered again by someone else. Duplicates are
// therefore rare but possible, and consumers dedupe on the event id, which a
// retry never changes. Losses are not possible once Enqueue's transaction
// commits.
//
// Ordering is per partition key while every publish succeeds. An event whose
// publish fails is retried later and can be overtaken by a younger event for
// the same aggregate; consumers detect that from aggregateversion (§9), as
// the standard requires them to.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/eventing/envelope"
)

// publish_state values (ZS-EVENT-001 §6).
const (
	StatePending     = "pending"
	StateClaimed     = "claimed"
	StatePublished   = "published"
	StateFailed      = "failed"
	StateQuarantined = "quarantined"
)

// last_error_code values. Structured so an alert or an operator query can
// group failures without parsing free text.
const (
	CodeBrokerUnavailable = "BROKER_UNAVAILABLE"
	CodePublishFailed     = "PUBLISH_FAILED"
	CodePermanent         = "PERMANENT_PUBLISH_ERROR"
	CodeInvalidStored     = "INVALID_STORED_ENVELOPE"
)

// EventIDHeader carries the event id as a Kafka header as well as in the body.
// general-ledger-svc's consumers-to-be were told to read it there before the
// id was in the body; keeping it costs nothing.
const EventIDHeader = "X-Event-ID"

// Enqueue writes env to the outbox inside tx, the caller's business
// transaction. It must be called on the same tx as the state change the event
// describes — that is the whole guarantee.
//
// A duplicate event id is an error, not a silent no-op: New allocates a fresh
// id per envelope, so a collision means the same envelope value was enqueued
// twice, which is a producer bug worth surfacing.
func Enqueue(ctx context.Context, tx pgx.Tx, env *envelope.Envelope) error {
	if tx == nil {
		return errors.New("outbox: enqueue needs the caller's transaction")
	}
	if env == nil {
		return errors.New("outbox: enqueue needs an envelope")
	}
	rendered, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("outbox: render %s: %w", env.Type, err)
	}
	outboxID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("outbox: allocate outbox id: %w", err)
	}
	var aggregateID *string
	if env.AggregateID != "" {
		aggregateID = &env.AggregateID
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO eventing_outbox
		    (outbox_id, event_id, event_type, schema_version, tenant_id, region,
		     aggregate_id, aggregate_version, partition_key, payload, payload_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		outboxID, env.ID, env.Type, env.SchemaVersion, env.TenantID, env.ResidencyRegion,
		aggregateID, env.AggregateVersion, env.PartitionKey(), rendered, env.PayloadHash,
	)
	if err != nil {
		return fmt.Errorf("outbox: enqueue %s: %w", env.Type, err)
	}
	return nil
}

// ── Broker seam ─────────────────────────────────────────────────────────────

// Header is one Kafka record header.
type Header struct {
	Key   string
	Value []byte
}

// Message is what the relay hands the broker.
type Message struct {
	Key     []byte
	Value   []byte
	Headers []Header
}

// Writer is what the relay needs from a Kafka producer. Declared here rather
// than importing a Kafka client, so this package does not pick the client for
// its callers and tests need no broker; each service adapts its kafka-go
// writer in a few lines.
type Writer interface {
	// WriteMessages must return only after the broker acknowledged every
	// message, or an error. A writer that returns before acknowledgement turns
	// the outbox's at-least-once guarantee into at-most-once.
	WriteMessages(ctx context.Context, msgs ...Message) error

	// Probe reports whether the broker is reachable and the target topic is
	// writable (for kafka-go: a metadata request for the topic that finds a
	// leader for its partitions), WITHOUT sending a message.
	//
	// It is how the relay tells a broker outage from a bad event. Inferring
	// that from which writes failed does not work: two poison events at the
	// head of the queue look exactly like an outage on every pass and stall
	// everything behind them, and a lone poison event can never be told apart
	// from one at all.
	Probe(ctx context.Context) error
}

// ErrPermanent marks a publish error that retrying cannot fix (the broker
// rejected the record itself: too large, invalid, not authorized for the
// topic). Wrap with Permanent in the Writer adapter; the relay quarantines
// such an event at once instead of spending its retry budget (§11 "Schema/
// contract ... Quarantine immediately").
var ErrPermanent = errors.New("outbox: permanent publish error")

// Permanent wraps err as permanent.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPermanent, err)
}

// ── Relay ───────────────────────────────────────────────────────────────────

// Config tunes the relay. DefaultConfig is a sound production starting point.
type Config struct {
	// BatchSize bounds one claim.
	BatchSize int
	// PollInterval is the sleep when a pass finds less than a full batch. A
	// full batch loops straight back, so a backlog drains at broker speed.
	PollInterval time.Duration
	// Lease is how long a claimed row is hidden from other relays. It must
	// comfortably exceed the writer's own timeout: a lease that expires while
	// the write is still in flight lets a second relay publish the row too.
	Lease time.Duration
	// MaxAttempts is how many event-specific failures an event may have before
	// it is quarantined (§11 "Poison event ... Move to controlled quarantine
	// after policy threshold; no infinite hot loop"). Broker-wide outages do
	// not count towards it — see drainOnce.
	MaxAttempts int
	// BaseBackoff doubles per attempt up to MaxBackoff, with ±20% jitter
	// (§11 "bounded exponential backoff/jitter").
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultConfig returns the recommended settings.
func DefaultConfig() Config {
	return Config{
		BatchSize:    100,
		PollInterval: time.Second,
		Lease:        2 * time.Minute,
		MaxAttempts:  12,
		BaseBackoff:  2 * time.Second,
		MaxBackoff:   5 * time.Minute,
	}
}

func (c Config) validate() error {
	switch {
	case c.BatchSize <= 0:
		return errors.New("outbox: BatchSize must be positive")
	case c.PollInterval <= 0:
		return errors.New("outbox: PollInterval must be positive")
	case c.Lease <= 0:
		return errors.New("outbox: Lease must be positive")
	case c.MaxAttempts <= 0:
		return errors.New("outbox: MaxAttempts must be positive")
	case c.BaseBackoff <= 0 || c.MaxBackoff < c.BaseBackoff:
		return errors.New("outbox: need 0 < BaseBackoff <= MaxBackoff")
	}
	return nil
}

// Relay delivers committed outbox rows to the broker.
//
// One Relay value must be driven by one goroutine (Run, or DrainOnce calls
// that do not overlap). Any number of Relay values — in one process or across
// replicas — may run against the same table concurrently.
type Relay struct {
	pool   *pgxpool.Pool
	writer Writer
	cfg    Config
	log    *zap.Logger

	// outagePasses counts consecutive passes that found the broker down, to
	// back off the whole relay rather than any one event.
	outagePasses int
}

// NewRelay builds a relay.
func NewRelay(pool *pgxpool.Pool, writer Writer, cfg Config, log *zap.Logger) (*Relay, error) {
	if pool == nil || writer == nil {
		return nil, errors.New("outbox: relay needs a pool and a writer")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Relay{pool: pool, writer: writer, cfg: cfg, log: log}, nil
}

// PassResult reports one drain pass.
type PassResult struct {
	Claimed     int
	Published   int
	Retrying    int // event-specific failure, retry scheduled
	Quarantined int
	Released    int // returned unclaimed because the broker looked down
}

// Run drains until ctx is cancelled. An unreachable broker is never fatal:
// events wait durably in Postgres, which is the reason they were stored first.
func (r *Relay) Run(ctx context.Context) {
	r.log.Info("eventing outbox relay started",
		zap.Int("batch_size", r.cfg.BatchSize), zap.Duration("lease", r.cfg.Lease))
	for {
		if ctx.Err() != nil {
			r.log.Info("eventing outbox relay stopped")
			return
		}
		res, err := r.DrainOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Warn("eventing outbox drain failed", zap.Error(err))
		}
		if err == nil && res.Claimed == r.cfg.BatchSize && res.Released == 0 {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.cfg.PollInterval):
		}
	}
}

// claimed is one leased row.
type claimed struct {
	outboxID     string
	eventID      string
	eventType    string
	partitionKey string
	payload      []byte
	attemptCount int
	dispatchedAt time.Time
	createdAt    time.Time
}

// DrainOnce runs a single claim → publish → record pass.
//
// TELLING A BROKER OUTAGE FROM A POISON EVENT. Counting every failed publish
// against the event would quarantine the entire backlog during a long Kafka
// outage, although nothing is wrong with any event — §11 says transient
// infrastructure failures are retried with backoff, not quarantined. So when
// the batch write fails the relay asks the broker directly (Writer.Probe):
//
//   - probe fails: an outage. Every record is released uncounted and the
//     whole relay backs off. No per-record writes are attempted, so a down
//     broker is not hit with a timeout per event.
//   - probe succeeds: the broker is up, so records are written one at a time
//     and each failure counts against that event. If two writes in a row fail
//     the broker is probed again, in case it went down mid-pass.
//
// A writer error wrapped with Permanent is always event-specific and
// quarantines at once.
func (r *Relay) DrainOnce(ctx context.Context) (PassResult, error) {
	var res PassResult
	batch, err := r.claim(ctx)
	if err != nil {
		return res, err
	}
	res.Claimed = len(batch)
	if len(batch) == 0 {
		return res, nil
	}

	msgs := make([]Message, 0, len(batch))
	ready := make([]claimed, 0, len(batch))
	for _, rec := range batch {
		value, err := envelope.WithPublishedAt(rec.payload, rec.dispatchedAt)
		if err != nil {
			// The row cannot be what Enqueue wrote. Retrying will not change
			// it, and sending it would hand consumers a malformed event.
			r.recordFailure(ctx, rec, true, CodeInvalidStored, err, &res)
			continue
		}
		msgs = append(msgs, message(rec, value))
		ready = append(ready, rec)
	}
	if len(ready) == 0 {
		return res, nil
	}

	// Fast path: one call for the whole batch. kafka-go's Writer batches, and
	// a synchronous one-message write waits out BatchTimeout per call —
	// identity-context-svc measured that as one event per second.
	batchErr := r.writer.WriteMessages(ctx, msgs...)
	if batchErr == nil {
		r.markPublished(ctx, ready, &res)
		r.outagePasses = 0
		return res, nil
	}
	if err := r.writer.Probe(ctx); err != nil {
		r.release(ctx, ready, fmt.Errorf("%w (broker probe: %v)", batchErr, err), &res)
		return res, nil
	}
	r.outagePasses = 0

	// Broker is up: one at a time, to blame precisely.
	consecutive := 0
	for i, rec := range ready {
		err := r.writer.WriteMessages(ctx, msgs[i])
		switch {
		case err == nil:
			consecutive = 0
			r.markPublished(ctx, []claimed{rec}, &res)
			continue
		case errors.Is(err, ErrPermanent):
			consecutive = 0
			r.recordFailure(ctx, rec, true, CodePermanent, err, &res)
			continue
		}
		consecutive++
		if consecutive >= 2 {
			if probeErr := r.writer.Probe(ctx); probeErr != nil {
				// Went down mid-pass: neither this event nor the rest is to blame.
				r.release(ctx, ready[i:], fmt.Errorf("%w (broker probe: %v)", err, probeErr), &res)
				return res, nil
			}
		}
		r.recordFailure(ctx, rec, false, CodePublishFailed, err, &res)
	}
	return res, nil
}

func message(rec claimed, value []byte) Message {
	return Message{
		Key:     []byte(rec.partitionKey),
		Value:   value,
		Headers: []Header{{Key: EventIDHeader, Value: []byte(rec.eventID)}},
	}
}

// claim leases up to BatchSize due rows in one statement.
//
// Due means pending/failed rows whose retry time has come, or claimed rows
// whose lease expired (their relay died). FOR UPDATE SKIP LOCKED inside the
// statement means two relays claiming at the same instant take disjoint rows;
// the lease means rows stay theirs after this transaction commits, while the
// publish is in flight — unlike a lock, which would have to be held across the
// broker round trip and would block every producer's enqueue behind a slow
// broker.
//
// dispatched_at becomes publishedat. It is re-stamped on every fresh claim,
// because a failed attempt never reached the broker; it is kept when a lease
// is reclaimed, because the dead relay's attempt may have.
func (r *Relay) claim(ctx context.Context) ([]claimed, error) {
	var out []claimed
	err := r.inRelayTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS (
			    SELECT outbox_id
			      FROM eventing_outbox
			     WHERE publish_state IN ('pending', 'failed', 'claimed')
			       AND CASE WHEN publish_state = 'claimed' THEN claimed_until <= now()
			                ELSE next_attempt_at <= now() END
			     ORDER BY created_at
			     LIMIT $1
			     FOR UPDATE SKIP LOCKED
			)
			UPDATE eventing_outbox o
			   SET dispatched_at = CASE WHEN o.publish_state = 'claimed'
			                            THEN COALESCE(o.dispatched_at, now())
			                            ELSE now() END,
			       publish_state = 'claimed',
			       claimed_until = now() + make_interval(secs => $2)
			  FROM due
			 WHERE o.outbox_id = due.outbox_id
			RETURNING o.outbox_id::text, o.event_id, o.event_type, o.partition_key,
			          o.payload, o.attempt_count, o.dispatched_at, o.created_at`,
			r.cfg.BatchSize, r.cfg.Lease.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c claimed
			if err := rows.Scan(&c.outboxID, &c.eventID, &c.eventType, &c.partitionKey,
				&c.payload, &c.attemptCount, &c.dispatchedAt, &c.createdAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: claim: %w", err)
	}
	// RETURNING has no defined order; publish oldest first.
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out, nil
}

// markPublished records delivery. If this write fails the events are already
// on the topic; their leases expire and they are delivered again, which is a
// duplicate (tolerated) rather than a loss.
func (r *Relay) markPublished(ctx context.Context, recs []claimed, res *PassResult) {
	if len(recs) == 0 {
		return
	}
	ids := make([]string, len(recs))
	for i, rec := range recs {
		ids[i] = rec.outboxID
	}
	err := r.inRelayTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE eventing_outbox
			   SET publish_state = 'published', published_at = now(), claimed_until = NULL,
			       last_error_code = NULL, last_error = NULL
			 WHERE outbox_id = ANY($1::uuid[]) AND publish_state = 'claimed'`, ids)
		return err
	})
	if err != nil {
		r.log.Error("eventing outbox: published but not recorded — will redeliver after the lease",
			zap.Int("events", len(ids)), zap.Error(err))
	}
	res.Published += len(recs)
}

// recordFailure counts an event-specific failure, quarantining when the
// budget is spent or the failure is permanent.
func (r *Relay) recordFailure(ctx context.Context, rec claimed, permanent bool, code string, cause error, res *PassResult) {
	quarantine := permanent || rec.attemptCount+1 >= r.cfg.MaxAttempts
	state := StateFailed
	if quarantine {
		state = StateQuarantined
	}
	err := r.inRelayTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE eventing_outbox
			   SET attempt_count = attempt_count + 1,
			       publish_state = $2,
			       claimed_until = NULL,
			       next_attempt_at = now() + make_interval(secs => $3),
			       last_error_code = $4,
			       last_error = $5
			 WHERE outbox_id = $1::uuid AND publish_state = 'claimed'`,
			rec.outboxID, state, r.backoff(rec.attemptCount).Seconds(), code, errText(cause))
		return err
	})
	if err != nil {
		r.log.Error("eventing outbox: could not record publish failure — lease will expire and retry",
			zap.String("event_id", rec.eventID), zap.Error(err))
		return
	}
	if quarantine {
		res.Quarantined++
		r.log.Error("eventing outbox: event quarantined",
			zap.String("event_id", rec.eventID), zap.String("event_type", rec.eventType),
			zap.String("code", code), zap.Int("attempts", rec.attemptCount+1), zap.Error(cause))
		return
	}
	res.Retrying++
	r.log.Warn("eventing outbox: publish failed, will retry",
		zap.String("event_id", rec.eventID), zap.Int("attempts", rec.attemptCount+1), zap.Error(cause))
}

// release returns rows uncounted after the broker looked down, scheduling
// them behind a relay-wide backoff.
func (r *Relay) release(ctx context.Context, recs []claimed, cause error, res *PassResult) {
	if len(recs) == 0 {
		return
	}
	retryIn := r.backoff(r.outagePasses)
	r.outagePasses++
	ids := make([]string, len(recs))
	for i, rec := range recs {
		ids[i] = rec.outboxID
	}
	err := r.inRelayTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE eventing_outbox
			   SET publish_state = CASE WHEN attempt_count = 0 THEN 'pending' ELSE 'failed' END,
			       claimed_until = NULL,
			       next_attempt_at = now() + make_interval(secs => $2),
			       last_error_code = $3,
			       last_error = $4
			 WHERE outbox_id = ANY($1::uuid[]) AND publish_state = 'claimed'`,
			ids, retryIn.Seconds(), CodeBrokerUnavailable, errText(cause))
		return err
	})
	if err != nil {
		r.log.Error("eventing outbox: could not release batch — leases will expire",
			zap.Int("events", len(ids)), zap.Error(err))
	}
	res.Released += len(recs)
	r.log.Warn("eventing outbox: broker unavailable, backing off",
		zap.Int("events", len(ids)), zap.Duration("retry_in", retryIn), zap.Error(cause))
}

// backoff is BaseBackoff·2^n capped at MaxBackoff, with ±20% jitter so many
// relays recovering from one outage do not retry in lock-step.
func (r *Relay) backoff(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	d := float64(r.cfg.BaseBackoff) * math.Pow(2, float64(n))
	// The cap check also catches overflow: past ~2^62 ns the float converts to
	// a negative Duration, which would schedule the retry in the past.
	if d <= 0 || d > float64(r.cfg.MaxBackoff) {
		d = float64(r.cfg.MaxBackoff)
	}
	return time.Duration(d * (0.8 + 0.4*rand.Float64()))
}

// inRelayTx runs fn in a transaction carrying the app.outbox_relay capability.
// A service that puts row-level security on eventing_outbox grants the relay
// access with a policy on current_setting('app.outbox_relay', true) = 'true';
// on a table without RLS the setting is inert.
func (r *Relay) inRelayTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 2000 {
		s = s[:2000]
	}
	return s
}

// ── Operations ──────────────────────────────────────────────────────────────

// Stats are the producer signals ZS-EVENT-001 §6.1 calls first-class.
type Stats struct {
	Backlog          int           // not yet published, not quarantined
	Quarantined      int           // needs an owner's decision (§11.1)
	OldestBacklogAge time.Duration // zero when the backlog is empty
}

// ReadStats reads the backlog signals. Wire them to a gauge and alert on a
// backlog age that only grows: it is the one sign that the relay stopped or
// the broker is gone while everything else about the service looks healthy.
func (r *Relay) ReadStats(ctx context.Context) (Stats, error) {
	var s Stats
	var ageSeconds float64
	err := r.inRelayTx(ctx, func(tx pgx.Tx) error {
		// Each subquery's predicate matches a partial index, so the cost
		// tracks the backlog, not the table's published history.
		return tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM eventing_outbox
			         WHERE publish_state IN ('pending', 'failed', 'claimed')),
			       (SELECT count(*) FROM eventing_outbox
			         WHERE publish_state = 'quarantined'),
			       (SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)
			          FROM eventing_outbox
			         WHERE publish_state IN ('pending', 'failed', 'claimed'))`).Scan(&s.Backlog, &s.Quarantined, &ageSeconds)
	})
	if err != nil {
		return Stats{}, fmt.Errorf("outbox: stats: %w", err)
	}
	s.OldestBacklogAge = time.Duration(ageSeconds * float64(time.Second))
	return s, nil
}

// Requeue returns a quarantined event to the queue after its owner fixed the
// cause (§11.1 disposition "fix-and-replay"). The event id is unchanged — it is
// a redelivery of the same fact, not a new one — and its attempt budget is
// reset. Reports false if the event is not quarantined.
func (r *Relay) Requeue(ctx context.Context, eventID string) (bool, error) {
	var n int64
	err := r.inRelayTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE eventing_outbox
			   SET publish_state = 'pending', attempt_count = 0, next_attempt_at = now(),
			       last_error_code = NULL, last_error = NULL
			 WHERE event_id = $1 AND publish_state = 'quarantined'`, eventID)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return false, fmt.Errorf("outbox: requeue %s: %w", eventID, err)
	}
	return n == 1, nil
}
