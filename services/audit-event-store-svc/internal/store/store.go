// Package store provides the append-only persistence layer for audit events.
//
// Architectural constraints (doctrine.md):
//   - No UPDATE or DELETE on any stored event — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//       INSERT INTO audit_events … ON CONFLICT (event_id) DO NOTHING
//     A prior SELECT-EXISTS check is explicitly prohibited: two concurrent
//     goroutines can both pass a SELECT EXISTS check before either inserts,
//     producing a duplicate row.  The ON CONFLICT clause makes the entire
//     upsert atomic at the database level.
//   - Every record carries tenant_id and legal_entity_id per doctrine §3.2.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// AuditEvent is the normalised representation stored in the audit_events table.
// Fields map 1-to-1 to the schema columns defined in
// deployments/migrations/000001_initial_schema.up.sql and
// deployments/migrations/000002_add_hash_chain_fields.up.sql.
type AuditEvent struct {
	// EventID is the globally unique identifier for this event occurrence,
	// assigned by the publishing service.  It is the deduplication key.
	EventID string

	// EventType mirrors the canonical event name (e.g. "identity.context.resolved").
	EventType string

	// Mandatory governance context — every record must be tenant- and entity-bound.
	TenantID      string
	LegalEntityID string

	// PrincipalID is optional — system events may have no human actor.
	PrincipalID string

	// CorrelationID / CausationID are optional provenance fields promoted to
	// first-class columns per docs/original_doc/zoiko_suite_doc3.txt rule
	// 15.4 ("hash-chain integrity") — see migration 000002 for the full
	// rationale. Not every event carries a causation_id today.
	CorrelationID string
	CausationID   string

	// Envelope provenance fields.
	SourceService string
	SchemaVersion string

	// Payload is the full event payload, stored as raw JSON for queryability.
	Payload json.RawMessage

	// SequenceNumber, PayloadHash, and PreviousEventHash are set by the Store
	// implementation at insert time — callers must never set these
	// themselves. They are populated on the AuditEvent passed to Store()
	// after a successful insert, and returned by Get(), so callers/tests can
	// verify the chain.
	SequenceNumber    int64
	PayloadHash       string
	PreviousEventHash string
}

// hashPayload returns the hex-encoded SHA-256 digest of raw. This is what
// makes a stored row's payload independently re-verifiable: recomputing
// this hash from the stored payload and comparing it to the stored
// payload_hash detects any post-insert tampering with the payload column.
func hashPayload(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Store is the write interface for the audit event store.
// Implementations MUST be idempotent: inserting the same EventID twice MUST
// succeed without error and without storing a duplicate row.
type Store interface {
	// Store persists e atomically. If a row with the same EventID already
	// exists the call is a no-op and returns nil — e's SequenceNumber,
	// PayloadHash, and PreviousEventHash are left unset in that case, since
	// no new chain link was created.
	//
	// e is a pointer so implementations can report back the hash-chain
	// values they computed at insert time, letting callers/tests verify the
	// chain without a separate read.
	Store(ctx context.Context, e *AuditEvent) error
}

// ───────────────────────────────────────────────────────────────────────────
// PostgreSQL implementation
// ───────────────────────────────────────────────────────────────────────────

// PgStore implements Store against a PostgreSQL database via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// NewPgStore returns a PgStore connected via pool.
func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// auditChainLockKey is the advisory-lock key used to serialise hash-chain
// writes. Any fixed int64 works — the value itself is arbitrary, it just
// needs to be stable and not collide with another subsystem's advisory
// locks in the same database. Computed once from a fixed string so it's
// human-traceable rather than a magic number.
var auditChainLockKey = func() int64 {
	sum := sha256.Sum256([]byte("audit-event-store-svc:hash-chain"))
	// pg_advisory_xact_lock takes a bigint; truncate to signed int64 range.
	return int64(sum[0])<<56 | int64(sum[1])<<48 | int64(sum[2])<<40 | int64(sum[3])<<32 |
		int64(sum[4])<<24 | int64(sum[5])<<16 | int64(sum[6])<<8 | int64(sum[7])
}()

// Store inserts e into audit_events atomically, and extends the store's
// tamper-evident hash chain (see migration 000002 for the full rationale).
//
// Two correctness properties must both hold, and they interact:
//
//  1. Idempotent dedup on event_id — the existing property, unchanged.
//     A duplicate delivery must be a silent no-op and must NOT consume a
//     chain link (that would waste a sequence_number but is otherwise
//     harmless; more importantly it must not corrupt the chain for anyone
//     else).
//  2. A gap-free, unambiguous hash chain — every newly inserted row's
//     previous_event_hash must equal the payload_hash of whichever row
//     currently has the highest sequence_number, with no fork: two
//     concurrent inserts must never both read the same "current last hash"
//     and link to it, which would fork the chain into two branches instead
//     of one linear sequence.
//
// Property 2 is why this can't just be one INSERT statement like before.
// The transaction takes a global advisory lock (pg_advisory_xact_lock) for
// its whole duration, which serialises every Store() call against every
// other one — the SELECT of "current chain tip" and the INSERT that
// extends it happen as one atomic step from the chain's point of view. The
// lock is released automatically on commit or rollback, including if the
// INSERT is a no-op due to ON CONFLICT DO NOTHING.
func (s *PgStore) Store(ctx context.Context, e *AuditEvent) error {
	payloadHash := hashPayload(e.Payload)

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", auditChainLockKey); err != nil {
			return fmt.Errorf("acquire chain lock: %w", err)
		}

		// Both the previous hash AND the next sequence_number are computed
		// here, explicitly, under the advisory lock — deliberately NOT via a
		// BIGSERIAL/IDENTITY default on the column. A Postgres sequence's
		// nextval() is consumed as soon as it's evaluated, before ON
		// CONFLICT DO NOTHING can veto the insert, which would permanently
		// burn a sequence number (and leave a real gap in the chain) on
		// every duplicate delivery. Computing MAX(sequence_number)+1 here
		// instead means a deduped duplicate costs nothing: it's simply
		// recomputed fresh, unused, and discarded on the next call.
		var (
			prevHash *string
			maxSeq   int64
		)
		err := tx.QueryRow(ctx,
			"SELECT payload_hash, sequence_number FROM audit_events ORDER BY sequence_number DESC LIMIT 1",
		).Scan(&prevHash, &maxSeq)
		if err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("read chain tip: %w", err)
		}
		// err == pgx.ErrNoRows means this is the very first row ever
		// inserted — prevHash stays nil and maxSeq stays 0, the documented
		// genesis case (first real sequence_number will be 1).
		nextSeq := maxSeq + 1

		const q = `
INSERT INTO audit_events
    (event_id, event_type, tenant_id, legal_entity_id, principal_id,
     source_service, schema_version, payload,
     correlation_id, causation_id, sequence_number, payload_hash, previous_event_hash)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (event_id) DO NOTHING
RETURNING sequence_number`

		row := tx.QueryRow(ctx, q,
			e.EventID,
			e.EventType,
			e.TenantID,
			e.LegalEntityID,
			nullableString(e.PrincipalID),
			e.SourceService,
			e.SchemaVersion,
			e.Payload,
			nullableString(e.CorrelationID),
			nullableString(e.CausationID),
			nextSeq,
			payloadHash,
			prevHash,
		)

		var seq int64
		if err := row.Scan(&seq); err != nil {
			if err == pgx.ErrNoRows {
				// ON CONFLICT DO NOTHING fired: duplicate event_id, no chain
				// link was created. nextSeq above is simply discarded —
				// never persisted, never "spent" — leave e's chain fields
				// unset and treat this exactly like the pre-existing dedup
				// no-op.
				return nil
			}
			return fmt.Errorf("insert audit event %q: %w", e.EventID, err)
		}

		e.SequenceNumber = seq
		e.PayloadHash = payloadHash
		if prevHash != nil {
			e.PreviousEventHash = *prevHash
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store audit event %q: %w", e.EventID, err)
	}

	s.log.Debug("audit event stored",
		zap.String("event_id", e.EventID),
		zap.String("event_type", e.EventType),
		zap.String("tenant_id", e.TenantID),
		zap.Int64("sequence_number", e.SequenceNumber),
	)
	return nil
}

// nullableString converts empty string to nil so Postgres stores NULL instead
// of an empty VARCHAR for optional fields like principal_id.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ───────────────────────────────────────────────────────────────────────────
// In-memory fake implementation (used in tests — no Postgres required)
// ───────────────────────────────────────────────────────────────────────────

// FakeStore is a thread-safe in-memory implementation of Store for use in
// unit tests.  It replicates the atomic-dedup semantic of the Postgres
// implementation: if the same EventID is inserted concurrently, exactly one
// row is stored and neither call returns an error. It also replicates the
// hash-chain semantics of PgStore (same lock-serialise-then-extend shape,
// using an in-process mutex instead of pg_advisory_xact_lock), so tests
// written against FakeStore exercise real chain behaviour, not a stub.
type FakeStore struct {
	mu       sync.Mutex
	events   map[string]AuditEvent
	order    []string // event_id in insertion order, for chain-tip lookup
	nextSeq  int64
	lastHash string // payload_hash of the current chain tip; "" if empty
}

// NewFakeStore returns an initialised FakeStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{events: make(map[string]AuditEvent)}
}

// Store inserts e into the in-memory map and extends the fake chain.
// If a row with the same EventID already exists the call is a no-op (DO
// NOTHING) and does not consume a chain link — mirroring PgStore.
// The mutex guarantees that concurrent inserts of the same EventID are
// serialised and only one wins, and that the chain-tip read + extend is
// atomic, replicating the database-level guarantees.
func (f *FakeStore) Store(_ context.Context, e *AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.events[e.EventID]; exists {
		return nil // idempotent: duplicate event_id → silent no-op
	}

	f.nextSeq++
	e.SequenceNumber = f.nextSeq
	e.PayloadHash = hashPayload(e.Payload)
	e.PreviousEventHash = f.lastHash

	f.events[e.EventID] = *e
	f.order = append(f.order, e.EventID)
	f.lastHash = e.PayloadHash
	return nil
}

// Count returns the number of stored events (test helper).
func (f *FakeStore) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// Get returns the stored event for the given EventID (test helper).
func (f *FakeStore) Get(eventID string) (AuditEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.events[eventID]
	return e, ok
}

// ChainInOrder returns every stored event in insertion (chain) order —
// a test helper for verifying the full hash chain end to end.
func (f *FakeStore) ChainInOrder() []AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]AuditEvent, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.events[id])
	}
	return out
}

// ─── compile-time interface check ──────────────────────────────────────────

// Ensure both implementations satisfy the Store interface at compile time.
var _ Store = (*PgStore)(nil)
var _ Store = (*FakeStore)(nil)

// ─── StoredAt helper (read-only, for tests that need insertion time) ───────

// StoredAt returns the current UTC time.  Used by tests that need to verify
// stored_at is populated — the real DB sets it via DEFAULT NOW().
func StoredAt() time.Time { return time.Now().UTC() }
