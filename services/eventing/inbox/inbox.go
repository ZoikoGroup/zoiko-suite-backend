// Package inbox is the shared consumer-side counterpart of the outbox
// (ZS-SVC-M-001 INT-03): durable deduplication and processing receipts.
//
// THE GUARANTEE. Transport is at-least-once, so every consumer sees
// duplicates. Process runs the consumer's business effect and records its
// receipt in ONE database transaction. A crash before commit leaves neither
// the effect nor the receipt, so redelivery simply retries; a crash after
// commit leaves both, so redelivery is recognised as a duplicate and does
// nothing. There is no window in which the effect exists without the receipt
// (INT-NP-007, INT-NP-010, INT-NP-013).
//
// Two concurrent deliveries of the same event serialize on the receipt row:
// the second waits for the first to finish and then sees it completed.
//
// What it does not do: an effect that leaves the database (an HTTP call, a
// payment) cannot join the transaction. Such a handler must use
// Receipt.IdempotencyKey as the external idempotency subject, so a retry
// resolves the same external operation instead of creating another.
package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/integration"
	"zoiko.io/eventing/repair"
)

// TableName is the one inbox table every consuming service creates.
const TableName = "eventing_inbox"

// RegionSQL adds the region column to an inbox created before it existed. It is additive and idempotent: append it
// to the consumer's next migration. Receipts written before it have a NULL region, which a region-pinned inbox refuses
// to repair (it cannot prove the event belongs here).
const RegionSQL = `ALTER TABLE eventing_inbox ADD COLUMN IF NOT EXISTS region TEXT;`

// SchemaSQL is copied verbatim into an adopting service's migration, together
// with repair.SchemaSQL.
const SchemaSQL = `
CREATE TABLE eventing_inbox (
    inbox_id          UUID         PRIMARY KEY,
    consumer          TEXT         NOT NULL,
    event_id          TEXT         NOT NULL,
    event_type        TEXT         NOT NULL,
    tenant_id         TEXT         NOT NULL,
    aggregate_id      TEXT,
    aggregate_version BIGINT,
    payload_hash      TEXT         NOT NULL,
    state             TEXT         NOT NULL DEFAULT 'processing',
    attempt_count     INTEGER      NOT NULL DEFAULT 1,
    duplicate_count   INTEGER      NOT NULL DEFAULT 0,
    received_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    result_ref        TEXT,
    last_error_code   TEXT,
    last_error        TEXT,
    -- the event's residency region, so a repair can be refused when it would move the event across regions (INT-NP-045)
    region            TEXT,

    CONSTRAINT eventing_inbox_unique_delivery UNIQUE (consumer, tenant_id, event_id),
    CONSTRAINT eventing_inbox_state_valid CHECK (state IN
        ('received', 'processing', 'completed', 'failed_retryable', 'quarantined', 'superseded')),
    CONSTRAINT eventing_inbox_attempts_nonneg CHECK (attempt_count >= 0 AND duplicate_count >= 0),
    CONSTRAINT eventing_inbox_completed_has_time CHECK
        (state NOT IN ('completed', 'superseded') OR completed_at IS NOT NULL)
);

-- Per-aggregate ordering check: highest completed version for an aggregate.
CREATE INDEX eventing_inbox_aggregate_idx
    ON eventing_inbox (consumer, tenant_id, aggregate_id, aggregate_version)
    WHERE state = 'completed' AND aggregate_id IS NOT NULL;

-- Everything that needs a decision, oldest first.
CREATE INDEX eventing_inbox_attention_idx ON eventing_inbox (updated_at)
    WHERE state IN ('failed_retryable', 'quarantined', 'processing', 'received');
`

// RLSSQL scopes the inbox to the tenant set by the transaction, following the
// convention of the services that already adopted the outbox.
const RLSSQL = `
ALTER TABLE eventing_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_inbox FORCE ROW LEVEL SECURITY;

CREATE POLICY eventing_inbox_tenant_isolation ON eventing_inbox FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
`

// States.
const (
	StateProcessing = "processing"
	StateCompleted  = "completed"
	StateFailed     = "failed_retryable"
	StateQuarantine = "quarantined"
	StateSuperseded = "superseded"
)

// Outcome is what Process did with a delivery.
type Outcome string

const (
	// Processed: the handler ran and its effect and receipt committed.
	Processed Outcome = "PROCESSED"
	// Duplicate: already completed earlier; the handler did not run.
	Duplicate Outcome = "DUPLICATE"
	// Stale: an equal or newer version of the aggregate was already applied;
	// the handler did not run, so state cannot regress (INT-NP-008).
	Stale Outcome = "STALE"
	// Held: the event is quarantined and waits for an approved repair.
	Held Outcome = "HELD"
)

// Message is the identity of one delivery.
type Message struct {
	EventID          string
	EventType        string
	TenantID         string
	AggregateID      string
	AggregateVersion *int64
	PayloadHash      string
	// Region is the event's residency region from its envelope; empty for a legacy envelope that names none.
	Region string
}

type wire struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	TenantID         string          `json:"tenantid"`
	AggregateID      string          `json:"aggregateid"`
	AggregateVersion *int64          `json:"aggregateversion"`
	PayloadHash      string          `json:"payloadhash"`
	Region           string          `json:"residencyregion"`
	LegacyID         string          `json:"event_id"`
	LegacyType       string          `json:"event_type"`
	LegacyTenant     string          `json:"tenant_id"`
	Data             json.RawMessage `json:"data"`
	LegacyPayload    json.RawMessage `json:"payload"`
}

// Parse reads a delivery's identity from a canonical (ZS-EVENT-001) envelope or
// a legacy one. When a producer sent no payload hash, one is computed over the
// payload bytes only, so a redelivery that differs merely in an emission
// timestamp is not mistaken for a conflicting event.
func Parse(raw []byte) (Message, error) {
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Message{}, fmt.Errorf("inbox: undecodable message: %w", err)
	}
	m := Message{
		EventID:          first(w.ID, w.LegacyID),
		EventType:        first(w.Type, w.LegacyType),
		TenantID:         first(w.TenantID, w.LegacyTenant),
		AggregateID:      w.AggregateID,
		AggregateVersion: w.AggregateVersion,
		PayloadHash:      w.PayloadHash,
		Region:           w.Region,
	}
	if m.PayloadHash == "" {
		body := w.Data
		if len(body) == 0 {
			body = w.LegacyPayload
		}
		sum := sha256.Sum256(body)
		m.PayloadHash = "sha256:" + hex.EncodeToString(sum[:])
	}
	return m, m.validate()
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ErrInvalidMessage means the delivery cannot be identified well enough to
// deduplicate. It must be quarantined or rejected by the caller, never
// processed without a receipt.
var ErrInvalidMessage = errors.New("inbox: message lacks event id, type, tenant or payload hash")

func (m Message) validate() error {
	if strings.TrimSpace(m.EventID) == "" || strings.TrimSpace(m.EventType) == "" ||
		strings.TrimSpace(m.TenantID) == "" || strings.TrimSpace(m.PayloadHash) == "" {
		return ErrInvalidMessage
	}
	if (m.AggregateID == "") != (m.AggregateVersion == nil) {
		return errors.New("inbox: aggregate id and aggregate version must be given together")
	}
	return nil
}

// Receipt is what a handler is told about the delivery it is processing.
type Receipt struct {
	Consumer string
	EventID  string
	TenantID string
	Attempt  int
	// IdempotencyKey is stable across redeliveries. Use it as the idempotency
	// subject of any effect that cannot join the transaction.
	IdempotencyKey string
}

// Handler performs the consumer's business effect inside tx. Returning an
// error rolls the effect back and records a failed attempt. The handler runs while
// a pooled connection is held and a receipt row may be locked, so it must use
// tx for every database read and write and must not borrow a second
// connection from the same pool (do such reads before calling Process). Wrap an error with
// Poison to quarantine at once instead of retrying.
type Handler func(ctx context.Context, tx pgx.Tx, r Receipt) (resultRef string, err error)

// ErrPoison marks an error that retrying cannot fix.
var ErrPoison = errors.New("inbox: poison message")

// Poison wraps err so Process quarantines the message immediately.
func Poison(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPoison, err)
}

// Result reports what Process did.
type Result struct {
	Outcome   Outcome
	ResultRef string
	// Gap is true when the aggregate version jumped past the highest one this
	// consumer applied. It is informational: a consumer may legitimately not
	// see every version of an aggregate.
	Gap bool
	// Attempt is the attempt number this delivery represents.
	Attempt int
	// FailureState is set only when Process returns an error from the handler:
	// StateFailed (will be retried) or StateQuarantine.
	FailureState string
}

// TxRunner runs fn in a transaction scoped to tenantID.
type TxRunner interface {
	InTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error
}

// PoolRunner is a TxRunner over a pgx pool. TenantSetting is the transaction
// local setting the service's RLS policies read (usually "app.tenant_id").
type PoolRunner struct {
	Pool          *pgxpool.Pool
	TenantSetting string
}

// InTx implements TxRunner.
func (p PoolRunner) InTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if p.TenantSetting != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", p.TenantSetting, tenantID); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Config tunes an Inbox.
type Config struct {
	// MaxAttempts is how many failed attempts a message may have before it is
	// quarantined (no infinite hot loop).
	MaxAttempts int
	// LockTimeout bounds how long a delivery waits for a concurrent delivery of
	// the same event (or aggregate) to finish. Zero means 30s. A wait that
	// outlasts it fails the delivery without recording anything, so it is
	// retried; it exists so that a handler which wrongly holds a second
	// connection cannot stall a whole pool forever.
	LockTimeout time.Duration
}

// DefaultConfig is a sound starting point.
func DefaultConfig() Config { return Config{MaxAttempts: 5, LockTimeout: 30 * time.Second} }

var consumerName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Inbox is one consumer's receipt store.
type Inbox struct {
	db       TxRunner
	consumer string
	cfg      Config
	region   string
}

// ErrRegionMismatch means a repair would act on an event whose residency region is not this inbox's (or is unknown).
var ErrRegionMismatch = errors.New("inbox: the event's residency region does not match this consumer's region")

// SetRegion pins the inbox to a residency region. Requeue then refuses any receipt whose event region differs, or is
// not recorded: a repair must not move an event out of, or into, a region it does not belong to (INT-REG-002, INT-NP-045).
// Unset means no region check. Call before serving.
func (i *Inbox) SetRegion(region string) { i.region = region }

// New builds an Inbox for the named consumer (a stable name: renaming it
// forgets every receipt).
func New(db TxRunner, consumer string, cfg Config) (*Inbox, error) {
	if db == nil {
		return nil, errors.New("inbox: needs a transaction runner")
	}
	if !consumerName.MatchString(consumer) {
		return nil, errors.New("inbox: consumer must be a lower-case identifier")
	}
	if cfg.MaxAttempts <= 0 {
		return nil, errors.New("inbox: MaxAttempts must be positive")
	}
	return &Inbox{db: db, consumer: consumer, cfg: cfg}, nil
}

type handlerFailure struct{ err error }

func (h handlerFailure) Error() string { return h.err.Error() }
func (h handlerFailure) Unwrap() error { return h.err }

// Process applies one delivery exactly once in effect.
//
// A conflicting redelivery (same event id, different payload hash) returns
// INT-IDEM-005 and changes nothing: it is not the same fact and must not be
// treated as a harmless duplicate.
func (i *Inbox) Process(ctx context.Context, msg Message, fn Handler) (Result, error) {
	if err := msg.validate(); err != nil {
		return Result{}, err
	}
	if fn == nil {
		return Result{}, errors.New("inbox: handler is required")
	}
	var res Result
	err := i.db.InTx(ctx, msg.TenantID, func(tx pgx.Tx) error {
		res = Result{}
		if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)", i.lockTimeoutMS()); err != nil {
			return err
		}
		r, err := i.begin(ctx, tx, msg)
		if err != nil {
			return err
		}
		res.Attempt = r.attempt
		switch r.disposition {
		case dispDuplicate:
			res.Outcome = Duplicate
			return nil
		case dispHeld:
			res.Outcome = Held
			return nil
		case dispStale:
			res.Outcome = Stale
			res.ResultRef = r.resultRef
			return nil
		}
		res.Gap = r.gap
		ref, err := fn(ctx, tx, Receipt{
			Consumer: i.consumer, EventID: msg.EventID, TenantID: msg.TenantID,
			Attempt: r.attempt, IdempotencyKey: i.consumer + ":" + msg.EventID,
		})
		if err != nil {
			return handlerFailure{err}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE eventing_inbox
			   SET state = 'completed', completed_at = now(), updated_at = now(),
			       result_ref = $4, last_error_code = NULL, last_error = NULL
			 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`,
			i.consumer, msg.TenantID, msg.EventID, nullable(ref)); err != nil {
			return err
		}
		res.Outcome = Processed
		res.ResultRef = ref
		return nil
	})
	if err == nil {
		return res, nil
	}
	var hf handlerFailure
	if !errors.As(err, &hf) {
		return Result{}, err
	}
	state, attempt, ferr := i.recordFailure(ctx, msg, hf.err)
	if ferr != nil {
		return Result{}, fmt.Errorf("inbox: handler failed (%v) and the failure could not be recorded: %w", hf.err, ferr)
	}
	return Result{Attempt: attempt, FailureState: state}, hf.err
}

const (
	dispProcess = iota
	dispDuplicate
	dispHeld
	dispStale
)

type beginResult struct {
	disposition int
	attempt     int
	gap         bool
	resultRef   string
}

func (i *Inbox) begin(ctx context.Context, tx pgx.Tx, msg Message) (beginResult, error) {
	// Serialize events of one aggregate so the ordering check below cannot be
	// raced by two deliveries of different versions.
	if msg.AggregateID != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			i.consumer+"|"+msg.TenantID+"|"+msg.AggregateID); err != nil {
			return beginResult{}, err
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return beginResult{}, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO eventing_inbox
		    (inbox_id, consumer, event_id, event_type, tenant_id, aggregate_id,
		     aggregate_version, payload_hash, state, attempt_count, region)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'processing',1,$9)
		ON CONFLICT (consumer, tenant_id, event_id) DO NOTHING`,
		id, i.consumer, msg.EventID, msg.EventType, msg.TenantID,
		nullable(msg.AggregateID), msg.AggregateVersion, msg.PayloadHash, nullable(msg.Region))
	if err != nil {
		return beginResult{}, err
	}
	attempt := 1
	if tag.RowsAffected() == 0 {
		var state, hash string
		var prevAttempts int
		// FOR UPDATE: a concurrent delivery of the same event waits here until
		// the first transaction finishes, then sees its outcome.
		err := tx.QueryRow(ctx, `
			SELECT state, payload_hash, attempt_count FROM eventing_inbox
			 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3 FOR UPDATE`,
			i.consumer, msg.TenantID, msg.EventID).Scan(&state, &hash, &prevAttempts)
		if err != nil {
			return beginResult{}, err
		}
		if hash != msg.PayloadHash {
			return beginResult{}, integration.New(integration.CodeIdempotencyConflict,
				"event id was already received with different content")
		}
		switch state {
		case StateCompleted, StateSuperseded:
			_, err := tx.Exec(ctx, `
				UPDATE eventing_inbox SET duplicate_count = duplicate_count + 1, updated_at = now()
				 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`,
				i.consumer, msg.TenantID, msg.EventID)
			return beginResult{disposition: dispDuplicate, attempt: prevAttempts}, err
		case StateQuarantine:
			return beginResult{disposition: dispHeld, attempt: prevAttempts}, nil
		}
		attempt = prevAttempts + 1
		if _, err := tx.Exec(ctx, `
			UPDATE eventing_inbox SET state = 'processing', attempt_count = $4, updated_at = now()
			 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`,
			i.consumer, msg.TenantID, msg.EventID, attempt); err != nil {
			return beginResult{}, err
		}
	}

	if msg.AggregateID == "" {
		return beginResult{disposition: dispProcess, attempt: attempt}, nil
	}
	var maxVersion *int64
	if err := tx.QueryRow(ctx, `
		SELECT max(aggregate_version) FROM eventing_inbox
		 WHERE consumer = $1 AND tenant_id = $2 AND aggregate_id = $3
		   AND state = 'completed' AND event_id <> $4`,
		i.consumer, msg.TenantID, msg.AggregateID, msg.EventID).Scan(&maxVersion); err != nil {
		return beginResult{}, err
	}
	if maxVersion == nil {
		return beginResult{disposition: dispProcess, attempt: attempt}, nil
	}
	if *msg.AggregateVersion <= *maxVersion {
		ref := fmt.Sprintf("stale: version %d <= applied %d", *msg.AggregateVersion, *maxVersion)
		_, err := tx.Exec(ctx, `
			UPDATE eventing_inbox
			   SET state = 'superseded', completed_at = now(), updated_at = now(), result_ref = $4
			 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`,
			i.consumer, msg.TenantID, msg.EventID, ref)
		return beginResult{disposition: dispStale, attempt: attempt, resultRef: ref}, err
	}
	return beginResult{disposition: dispProcess, attempt: attempt, gap: *msg.AggregateVersion > *maxVersion+1}, nil
}

// recordFailure records a failed attempt in its own transaction (the handler's
// transaction rolled back, taking its effect with it).
func (i *Inbox) recordFailure(ctx context.Context, msg Message, cause error) (state string, attempt int, err error) {
	permanent := errors.Is(cause, ErrPoison)
	code := "HANDLER_FAILED"
	if permanent {
		code = "POISON_MESSAGE"
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", 0, err
	}
	err = i.db.InTx(ctx, msg.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO eventing_inbox
			    (inbox_id, consumer, event_id, event_type, tenant_id, aggregate_id,
			     aggregate_version, payload_hash, state, attempt_count, last_error_code, last_error, region)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,
			        CASE WHEN $9::boolean OR 1 >= $10::int THEN 'quarantined' ELSE 'failed_retryable' END,
			        1, $11, $12, $13)
			ON CONFLICT (consumer, tenant_id, event_id) DO UPDATE
			   SET attempt_count = eventing_inbox.attempt_count + 1,
			       state = CASE WHEN $9::boolean OR eventing_inbox.attempt_count + 1 >= $10::int
			                    THEN 'quarantined' ELSE 'failed_retryable' END,
			       last_error_code = $11, last_error = $12, updated_at = now()
			 WHERE eventing_inbox.state NOT IN ('completed', 'superseded')
			RETURNING state, attempt_count`,
			id, i.consumer, msg.EventID, msg.EventType, msg.TenantID, nullable(msg.AggregateID),
			msg.AggregateVersion, msg.PayloadHash, permanent, i.cfg.MaxAttempts, code, safeError(cause), nullable(msg.Region),
		).Scan(&state, &attempt)
	})
	return state, attempt, err
}

// safeError keeps stored error text short and free of secrets.
func safeError(err error) string {
	s := integration.ScrubString(err.Error())
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── Operations and queries ──────────────────────────────────────────────────

// ErrNotFound means there is no receipt for the event.
var ErrNotFound = errors.New("inbox: no receipt for that event")

// ErrNotRepairable means the receipt is not in a state a repair applies to.
var ErrNotRepairable = errors.New("inbox: only quarantined or failed messages can be requeued")

// Record is one receipt (GetInboxReceipt / GetProcessingEvidence).
type Record struct {
	InboxID          string
	Consumer         string
	EventID          string
	EventType        string
	TenantID         string
	AggregateID      string
	AggregateVersion *int64
	PayloadHash      string
	State            string
	AttemptCount     int
	DuplicateCount   int
	ReceivedAt       time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
	ResultRef        string
	LastErrorCode    string
	LastError        string
}

const recordColumns = `inbox_id::text, consumer, event_id, event_type, tenant_id, COALESCE(aggregate_id,''),
	aggregate_version, payload_hash, state, attempt_count, duplicate_count, received_at, updated_at,
	completed_at, COALESCE(result_ref,''), COALESCE(last_error_code,''), COALESCE(last_error,'')`

func scanRecord(row pgx.Row) (*Record, error) {
	var r Record
	err := row.Scan(&r.InboxID, &r.Consumer, &r.EventID, &r.EventType, &r.TenantID, &r.AggregateID,
		&r.AggregateVersion, &r.PayloadHash, &r.State, &r.AttemptCount, &r.DuplicateCount,
		&r.ReceivedAt, &r.UpdatedAt, &r.CompletedAt, &r.ResultRef, &r.LastErrorCode, &r.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Get returns the receipt for an event.
func (i *Inbox) Get(ctx context.Context, tenantID, eventID string) (*Record, error) {
	var rec *Record
	err := i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		rec, err = scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM eventing_inbox
			WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`, i.consumer, tenantID, eventID))
		return err
	})
	return rec, err
}

// ListStuck returns receipts that are failed, quarantined or stuck in
// processing and have not changed for at least olderThan (ListStuck in §4.3).
func (i *Inbox) ListStuck(ctx context.Context, tenantID string, olderThan time.Duration, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Record
	err := i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+recordColumns+` FROM eventing_inbox
			 WHERE consumer = $1 AND tenant_id = $2
			   AND state IN ('failed_retryable', 'quarantined', 'processing', 'received')
			   AND updated_at <= now() - make_interval(secs => $3)
			 ORDER BY updated_at LIMIT $4`, i.consumer, tenantID, olderThan.Seconds(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// Stats are the consumer-side signals.
type Stats struct {
	Completed          int
	Failed             int
	Quarantined        int
	Superseded         int
	Duplicates         int           // total duplicate deliveries absorbed
	OldestAttentionAge time.Duration // age of the oldest failed/quarantined receipt
}

// ReadStats summarises a tenant's receipts for this consumer.
func (i *Inbox) ReadStats(ctx context.Context, tenantID string) (Stats, error) {
	var s Stats
	var age float64
	err := i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE state = 'completed'),
			       count(*) FILTER (WHERE state = 'failed_retryable'),
			       count(*) FILTER (WHERE state = 'quarantined'),
			       count(*) FILTER (WHERE state = 'superseded'),
			       COALESCE(sum(duplicate_count), 0),
			       COALESCE(EXTRACT(EPOCH FROM now() - min(updated_at)
			                FILTER (WHERE state IN ('failed_retryable', 'quarantined'))), 0)
			  FROM eventing_inbox WHERE consumer = $1 AND tenant_id = $2`, i.consumer, tenantID).
			Scan(&s.Completed, &s.Failed, &s.Quarantined, &s.Superseded, &s.Duplicates, &age)
	})
	s.OldestAttentionAge = time.Duration(age * float64(time.Second))
	return s, err
}

// Quarantine holds a delivery for an owner's decision without processing it
// (QuarantineProcessing). It never overwrites a receipt that already
// completed: a quarantine cannot undo an applied effect.
func (i *Inbox) Quarantine(ctx context.Context, msg Message, code, reason string) error {
	if err := msg.validate(); err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	return i.db.InTx(ctx, msg.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO eventing_inbox
			    (inbox_id, consumer, event_id, event_type, tenant_id, aggregate_id,
			     aggregate_version, payload_hash, state, attempt_count, last_error_code, last_error, region)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'quarantined',0,$9,$10,$11)
			ON CONFLICT (consumer, tenant_id, event_id) DO UPDATE
			   SET state = 'quarantined', last_error_code = $9, last_error = $10, updated_at = now()
			 WHERE eventing_inbox.state NOT IN ('completed', 'superseded')`,
			id, i.consumer, msg.EventID, msg.EventType, msg.TenantID, nullable(msg.AggregateID),
			msg.AggregateVersion, msg.PayloadHash, code, safeReason(reason), nullable(msg.Region))
		return err
	})
}

func safeReason(s string) string {
	s = integration.ScrubString(s)
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

// Requeue returns a quarantined or failed receipt to a retryable state so the
// next delivery is processed again (ReplayControlled). It needs an approved
// repair, is recorded in the append-only repair log in the same transaction,
// and cannot change what the receipt says about the payload: only its state
// and attempt budget.
func (i *Inbox) Requeue(ctx context.Context, tenantID, eventID string, auth repair.Authorization) error {
	if err := auth.Validate(); err != nil {
		return err
	}
	return i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		var state string
		var region *string
		err := tx.QueryRow(ctx, `SELECT state, region FROM eventing_inbox
			WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3 FOR UPDATE`,
			i.consumer, tenantID, eventID).Scan(&state, &region)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != StateQuarantine && state != StateFailed {
			return ErrNotRepairable
		}
		if i.region != "" && (region == nil || *region != i.region) {
			return ErrRegionMismatch
		}
		if err := repair.Record(ctx, tx, repair.Entry{Kind: "inbox", Consumer: i.consumer,
			TenantID: tenantID, EventID: eventID, Action: "requeue", PriorState: state, Auth: auth}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE eventing_inbox
			   SET state = 'failed_retryable', attempt_count = 0, updated_at = now(),
			       last_error_code = NULL, last_error = NULL
			 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`, i.consumer, tenantID, eventID)
		return err
	})
}

// Thresholds are the consumer-side alerting limits.
type Thresholds struct {
	MaxFailed int // 0 = unchecked
	// Owner is stamped on every alert; empty means DefaultAlertOwner.
	Owner string
}

// Alert is a canonical integration event the service should emit.
type Alert struct {
	Event  string // ZS-SVC-M-001 §10 event name
	Detail string
	// Owner is the queue that owns what the alert is about, so it routes somewhere (INT-CTRL-034). Set from
	// Thresholds.Owner, DefaultAlertOwner when that is empty.
	Owner string
}

// DefaultAlertOwner owns an alert when the service names no one: quarantine and dead letters are never ownerless.
const DefaultAlertOwner = "integration-operations"

// Evaluate turns Stats into the §10 alerts. inbox_processing_quarantined fires
// whenever anything is quarantined (it always needs an owner); the per-delivery
// inbox_duplicate_detected is raised by the consumer when Process reports
// Duplicate.
func Evaluate(s Stats, t Thresholds) []Alert {
	out := evaluate(s, t)
	owner := t.Owner
	if owner == "" {
		owner = DefaultAlertOwner
	}
	for i := range out {
		out[i].Owner = owner
	}
	return out
}

func evaluate(s Stats, t Thresholds) []Alert {
	var out []Alert
	if s.Quarantined > 0 {
		out = append(out, Alert{Event: "inbox_processing_quarantined", Detail: fmt.Sprintf("%d received events are quarantined", s.Quarantined)})
	}
	if t.MaxFailed > 0 && s.Failed > t.MaxFailed {
		out = append(out, Alert{Event: "inbox_processing_failing", Detail: fmt.Sprintf("%d received events are failing and being retried", s.Failed)})
	}
	return out
}

// requiredColumns is what this package reads and writes.
var requiredColumns = []string{
	"inbox_id", "consumer", "event_id", "event_type", "tenant_id", "aggregate_id",
	"aggregate_version", "payload_hash", "state", "attempt_count", "duplicate_count",
	"received_at", "updated_at", "completed_at", "result_ref", "last_error_code", "last_error", "region",
}

// VerifySchema fails if the inbox table or any column this package uses is
// missing from the connection's search_path. Call it at startup beside the DB
// ping: otherwise a service whose migration was never applied starts normally
// and fails on its first delivery.
func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		 WHERE table_name = $1 AND table_schema = ANY (current_schemas(false))`, TableName)
	if err != nil {
		return fmt.Errorf("inbox: verify schema: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return fmt.Errorf("inbox: verify schema: %w", err)
		}
		have[c] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inbox: verify schema: %w", err)
	}
	if len(have) == 0 {
		return fmt.Errorf("inbox: table %s does not exist — apply the eventing inbox migration", TableName)
	}
	var missing []string
	for _, c := range requiredColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("inbox: table %s is missing columns: %s", TableName, strings.Join(missing, ", "))
	}
	return nil
}

func (i *Inbox) lockTimeoutMS() string {
	d := i.cfg.LockTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	return strconv.FormatInt(d.Milliseconds(), 10)
}
