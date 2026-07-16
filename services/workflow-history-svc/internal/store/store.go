// Package store provides the append-only persistence layer for workflow history events.
//
// Architectural constraints (doctrine.md):
//   - No UPDATE or DELETE on any stored event — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//       INSERT INTO workflow_history_events … ON CONFLICT (event_id) DO NOTHING
//     A prior SELECT-EXISTS check is explicitly prohibited: two concurrent
//     goroutines can both pass a SELECT EXISTS check before either inserts,
//     producing a duplicate row. The ON CONFLICT clause makes the entire
//     upsert atomic at the database level.
//   - Every record carries tenant_id and legal_entity_id per doctrine §3.2.
//     For non-started events these are inherited from the workflow.started row
//     via GetTenantContext — the consumer is responsible for this lookup.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// WorkflowHistoryEvent is the normalised representation stored in the
// workflow_history_events table. Fields map 1-to-1 to the schema columns
// defined in deployments/migrations/000001_initial_schema.up.sql.
type WorkflowHistoryEvent struct {
	// EventID is the globally unique identifier for this event occurrence,
	// assigned by the publishing service. It is the deduplication key.
	EventID string

	// WorkflowInstanceID is the workflow instance this event belongs to.
	WorkflowInstanceID string

	// EventType is the canonical event name (e.g. "workflow.started").
	EventType string

	// CorrelationID is propagated from the event envelope.
	CorrelationID string

	// Mandatory governance context — every record must be tenant- and entity-bound.
	TenantID      string
	LegalEntityID string

	// Payload is the full event payload, stored as raw JSON for queryability.
	Payload json.RawMessage

	// RecordedAt is set by the database, not the consumer.
	RecordedAt time.Time
}

// TenantContext holds the tenant/entity context for a workflow instance,
// retrieved from the workflow.started event row.
type TenantContext struct {
	TenantID      string
	LegalEntityID string
}

// QueryFilter is the filter for cross-workflow history queries.
type QueryFilter struct {
	TenantID      string
	LegalEntityID string
	From          time.Time
	To            time.Time
}

// AppendStore is the write interface for the workflow history store.
// Implementations MUST be idempotent: inserting the same EventID twice MUST
// succeed without error and without storing a duplicate row.
type AppendStore interface {
	// Append persists e atomically. If a row with the same EventID already
	// exists the call is a no-op and returns nil.
	Append(ctx context.Context, e WorkflowHistoryEvent) error
}

// ReadStore is the read interface for the workflow history store.
type ReadStore interface {
	// ListByInstance returns all events for the given workflow instance,
	// ordered chronologically (recorded_at ASC).
	// Returns an empty slice (not an error) if no events exist.
	ListByInstance(ctx context.Context, workflowInstanceID string) ([]WorkflowHistoryEvent, error)

	// ListByFilter returns events matching the given filter ordered by
	// recorded_at ASC. All filter fields are required.
	ListByFilter(ctx context.Context, f QueryFilter) ([]WorkflowHistoryEvent, error)

	// GetTenantContext retrieves the tenant_id and legal_entity_id recorded
	// in the earliest event for the given workflow instance. This is used by
	// the consumer to inherit governance context for non-started events that
	// do not carry tenant/entity IDs in their payload.
	// Returns a zero-value TenantContext and false if no rows exist yet.
	GetTenantContext(ctx context.Context, workflowInstanceID string) (TenantContext, bool, error)
}

// ───────────────────────────────────────────────────────────────────────────
// PostgreSQL implementation
// ───────────────────────────────────────────────────────────────────────────

// PgStore implements AppendStore and ReadStore against a PostgreSQL database via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// NewPgStore returns a PgStore connected via pool.
func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Append inserts e into workflow_history_events atomically.
//
// The critical dedup guarantee is expressed in a single SQL statement:
//
//	INSERT INTO workflow_history_events … ON CONFLICT (event_id) DO NOTHING
//
// This is the ONLY safe pattern. A prior "SELECT … WHERE event_id = $1"
// followed by a conditional INSERT has a TOCTOU race under concurrent delivery:
// two goroutines can both observe "not exists" before either inserts, and both
// will proceed to insert, creating a duplicate row.
//
// With ON CONFLICT DO NOTHING the database itself serialises the decision at
// the PRIMARY KEY constraint level — exactly one insert wins, the other is
// silently discarded, and both callers get nil back.
func (s *PgStore) Append(ctx context.Context, e WorkflowHistoryEvent) error {
	const q = `
INSERT INTO workflow_history_events
    (event_id, workflow_instance_id, event_type, correlation_id,
     tenant_id, legal_entity_id, payload)
VALUES
    ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (event_id) DO NOTHING`

	_, err := s.pool.Exec(ctx, q,
		e.EventID,
		e.WorkflowInstanceID,
		e.EventType,
		e.CorrelationID,
		e.TenantID,
		e.LegalEntityID,
		e.Payload,
	)
	if err != nil {
		return fmt.Errorf("append workflow history event %q: %w", e.EventID, err)
	}
	s.log.Debug("workflow history event appended",
		zap.String("event_id", e.EventID),
		zap.String("event_type", e.EventType),
		zap.String("workflow_instance_id", e.WorkflowInstanceID),
	)
	return nil
}

// ListByInstance returns all events for the given workflowInstanceID ordered
// chronologically by recorded_at ASC.
func (s *PgStore) ListByInstance(ctx context.Context, workflowInstanceID string) ([]WorkflowHistoryEvent, error) {
	const q = `
SELECT event_id, workflow_instance_id, event_type, correlation_id,
       tenant_id, legal_entity_id, payload, recorded_at
FROM workflow_history_events
WHERE workflow_instance_id = $1
ORDER BY recorded_at ASC`

	rows, err := s.pool.Query(ctx, q, workflowInstanceID)
	if err != nil {
		return nil, fmt.Errorf("list workflow history by instance %q: %w", workflowInstanceID, err)
	}
	defer rows.Close()

	return scanRows(rows)
}

// ListByFilter returns events matching the given filter ordered by recorded_at ASC.
func (s *PgStore) ListByFilter(ctx context.Context, f QueryFilter) ([]WorkflowHistoryEvent, error) {
	const q = `
SELECT event_id, workflow_instance_id, event_type, correlation_id,
       tenant_id, legal_entity_id, payload, recorded_at
FROM workflow_history_events
WHERE tenant_id = $1
  AND legal_entity_id = $2
  AND recorded_at >= $3
  AND recorded_at <= $4
ORDER BY recorded_at ASC`

	rows, err := s.pool.Query(ctx, q, f.TenantID, f.LegalEntityID, f.From, f.To)
	if err != nil {
		return nil, fmt.Errorf("list workflow history by filter: %w", err)
	}
	defer rows.Close()

	return scanRows(rows)
}

// GetTenantContext retrieves the tenant_id and legal_entity_id from the earliest
// stored event for the given workflow instance. Returns (zero, false, nil) if no
// row exists yet.
func (s *PgStore) GetTenantContext(ctx context.Context, workflowInstanceID string) (TenantContext, bool, error) {
	const q = `
SELECT tenant_id, legal_entity_id
FROM workflow_history_events
WHERE workflow_instance_id = $1
ORDER BY recorded_at ASC
LIMIT 1`

	var tc TenantContext
	err := s.pool.QueryRow(ctx, q, workflowInstanceID).Scan(&tc.TenantID, &tc.LegalEntityID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return TenantContext{}, false, nil
		}
		return TenantContext{}, false, fmt.Errorf("get tenant context for instance %q: %w", workflowInstanceID, err)
	}
	return tc, true, nil
}

func scanRows(rows pgx.Rows) ([]WorkflowHistoryEvent, error) {
	var events []WorkflowHistoryEvent
	for rows.Next() {
		var e WorkflowHistoryEvent
		if err := rows.Scan(
			&e.EventID,
			&e.WorkflowInstanceID,
			&e.EventType,
			&e.CorrelationID,
			&e.TenantID,
			&e.LegalEntityID,
			&e.Payload,
			&e.RecordedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow history row: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow history rows: %w", err)
	}
	return events, nil
}

// ───────────────────────────────────────────────────────────────────────────
// In-memory fake implementation (used in tests — no Postgres required)
// ───────────────────────────────────────────────────────────────────────────

// FakeStore is a thread-safe in-memory implementation of AppendStore and
// ReadStore for use in unit tests. It replicates the atomic-dedup semantic
// of the Postgres implementation: if the same EventID is inserted concurrently,
// exactly one row is stored and neither call returns an error.
type FakeStore struct {
	mu     sync.Mutex
	events []WorkflowHistoryEvent
	index  map[string]struct{} // event_id dedup set
}

// NewFakeStore returns an initialised FakeStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{index: make(map[string]struct{})}
}

// Append inserts e into the in-memory slice.
// If a row with the same EventID already exists the call is a no-op (DO NOTHING).
func (f *FakeStore) Append(_ context.Context, e WorkflowHistoryEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.index[e.EventID]; exists {
		return nil // idempotent: duplicate event_id → silent no-op
	}
	if e.RecordedAt.IsZero() {
		e.RecordedAt = time.Now().UTC()
	}
	f.index[e.EventID] = struct{}{}
	f.events = append(f.events, e)
	return nil
}

// ListByInstance returns all events for the given workflowInstanceID ordered
// by RecordedAt ASC (insertion order in the fake, since Append stamps RecordedAt).
func (f *FakeStore) ListByInstance(_ context.Context, workflowInstanceID string) ([]WorkflowHistoryEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []WorkflowHistoryEvent
	for _, e := range f.events {
		if e.WorkflowInstanceID == workflowInstanceID {
			out = append(out, e)
		}
	}
	return out, nil
}

// ListByFilter returns events matching the given filter, ordered by RecordedAt ASC.
func (f *FakeStore) ListByFilter(_ context.Context, filter QueryFilter) ([]WorkflowHistoryEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []WorkflowHistoryEvent
	for _, e := range f.events {
		if e.TenantID == filter.TenantID &&
			e.LegalEntityID == filter.LegalEntityID &&
			!e.RecordedAt.Before(filter.From) &&
			!e.RecordedAt.After(filter.To) {
			out = append(out, e)
		}
	}
	return out, nil
}

// GetTenantContext returns the tenant context from the first event for the instance.
func (f *FakeStore) GetTenantContext(_ context.Context, workflowInstanceID string) (TenantContext, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.WorkflowInstanceID == workflowInstanceID {
			return TenantContext{TenantID: e.TenantID, LegalEntityID: e.LegalEntityID}, true, nil
		}
	}
	return TenantContext{}, false, nil
}

// Count returns the total number of stored events (test helper).
func (f *FakeStore) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// ─── compile-time interface checks ─────────────────────────────────────────

var _ AppendStore = (*PgStore)(nil)
var _ ReadStore = (*PgStore)(nil)
var _ AppendStore = (*FakeStore)(nil)
var _ ReadStore = (*FakeStore)(nil)
