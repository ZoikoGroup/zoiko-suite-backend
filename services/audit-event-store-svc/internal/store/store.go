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
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// AuditEvent is the normalised representation stored in the audit_events table.
// Fields map 1-to-1 to the schema columns defined in
// deployments/migrations/000001_initial_schema.up.sql.
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

	// Envelope provenance fields.
	SourceService string
	SchemaVersion string

	// Payload is the full event payload, stored as raw JSON for queryability.
	Payload json.RawMessage
}

// Store is the write interface for the audit event store.
// Implementations MUST be idempotent: inserting the same EventID twice MUST
// succeed without error and without storing a duplicate row.
type Store interface {
	// Store persists e atomically.  If a row with the same EventID already
	// exists the call is a no-op and returns nil.
	Store(ctx context.Context, e AuditEvent) error
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

// Store inserts e into audit_events atomically.
//
// The critical dedup guarantee is expressed in a single SQL statement:
//
//	INSERT INTO audit_events … ON CONFLICT (event_id) DO NOTHING
//
// This is the ONLY safe pattern.  A prior "SELECT … WHERE event_id = $1"
// followed by a conditional INSERT has a TOCTOU race under concurrent delivery:
// two goroutines can both observe "not exists" before either inserts, and both
// will proceed to insert, creating a duplicate row.
//
// With ON CONFLICT DO NOTHING the database itself serialises the decision at
// the PRIMARY KEY constraint level — exactly one insert wins, the other is
// silently discarded, and both callers get nil back.
func (s *PgStore) Store(ctx context.Context, e AuditEvent) error {
	const q = `
INSERT INTO audit_events
    (event_id, event_type, tenant_id, legal_entity_id, principal_id,
     source_service, schema_version, payload)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (event_id) DO NOTHING`

	_, err := s.pool.Exec(ctx, q,
		e.EventID,
		e.EventType,
		e.TenantID,
		e.LegalEntityID,
		nullableString(e.PrincipalID),
		e.SourceService,
		e.SchemaVersion,
		e.Payload,
	)
	if err != nil {
		return fmt.Errorf("store audit event %q: %w", e.EventID, err)
	}
	s.log.Debug("audit event stored",
		zap.String("event_id", e.EventID),
		zap.String("event_type", e.EventType),
		zap.String("tenant_id", e.TenantID),
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
// row is stored and neither call returns an error.
type FakeStore struct {
	mu     sync.Mutex
	events map[string]AuditEvent
}

// NewFakeStore returns an initialised FakeStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{events: make(map[string]AuditEvent)}
}

// Store inserts e into the in-memory map.
// If a row with the same EventID already exists the call is a no-op (DO NOTHING).
// The mutex guarantees that concurrent inserts of the same EventID are serialised
// and only one wins — replicating the database-level ON CONFLICT DO NOTHING guarantee.
func (f *FakeStore) Store(_ context.Context, e AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.events[e.EventID]; exists {
		return nil // idempotent: duplicate event_id → silent no-op
	}
	f.events[e.EventID] = e
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

// ─── compile-time interface check ──────────────────────────────────────────

// Ensure both implementations satisfy the Store interface at compile time.
var _ Store = (*PgStore)(nil)
var _ Store = (*FakeStore)(nil)

// ─── StoredAt helper (read-only, for tests that need insertion time) ───────

// StoredAt returns the current UTC time.  Used by tests that need to verify
// stored_at is populated — the real DB sets it via DEFAULT NOW().
func StoredAt() time.Time { return time.Now().UTC() }
