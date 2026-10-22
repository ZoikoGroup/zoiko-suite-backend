// Package store provides the append-only persistence layer for workflow history events.
//
// Architectural constraints (doctrine.md):
//   - No UPDATE or DELETE on any stored event — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//     INSERT INTO workflow_history_events … ON CONFLICT (event_id) DO NOTHING
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
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// EventTypeWorkflowStarted is the one event type that carries tenant_id and
// legal_entity_id, and therefore the only authoritative source of that context
// for the rest of an instance's events.
const EventTypeWorkflowStarted = "workflow.started"

// ErrAmbiguousTenantContext is returned by GetTenantContext when the
// workflow.started rows recorded for one workflow_instance_id disagree about
// tenant or legal entity. The schema keys rows on event_id only, so it does not
// prevent two tenants from emitting the same instance id; the service refuses
// to guess which one a later event belongs to.
var ErrAmbiguousTenantContext = errors.New("workflow.started rows for this workflow_instance_id disagree on tenant or legal entity")

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

// PageCursor is the keyset position a page starts after: the last row of the
// previous page, in the store's total order (recorded_at, then event_id).
type PageCursor struct {
	RecordedAt time.Time
	EventID    string
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
	// ListByInstance returns all events for the given workflow instance
	// scoped to tenantID, ordered chronologically (recorded_at ASC, then
	// event_id ASC so that rows sharing a timestamp have one stable order).
	// Returns an empty slice (not an error) if no events exist, including
	// when the instance exists but belongs to a different tenant.
	ListByInstance(ctx context.Context, tenantID, workflowInstanceID string) ([]WorkflowHistoryEvent, error)

	// ListByInstancePage is ListByInstance restricted to the rows strictly after
	// `after` (nil = from the start), at most limit rows, in the same total
	// order. Tenant scoping is applied to every page.
	ListByInstancePage(ctx context.Context, tenantID, workflowInstanceID string, after *PageCursor, limit int) ([]WorkflowHistoryEvent, error)

	// ListByFilter returns events matching the given filter ordered by
	// recorded_at ASC, then event_id ASC. All filter fields are required.
	ListByFilter(ctx context.Context, f QueryFilter) ([]WorkflowHistoryEvent, error)

	// ListByFilterPage is ListByFilter restricted to the rows strictly after
	// `after` (nil = from the start), at most limit rows, in the same total order.
	ListByFilterPage(ctx context.Context, f QueryFilter, after *PageCursor, limit int) ([]WorkflowHistoryEvent, error)

	// GetTenantContext retrieves the tenant_id and legal_entity_id recorded on
	// the workflow.started row(s) for the given workflow instance. Only
	// workflow.started rows are consulted — never a transition row, whatever
	// its age. This is used by the consumer to inherit governance context for
	// non-started events that do not carry tenant/entity IDs in their payload.
	// Returns a zero-value TenantContext and false if no workflow.started row
	// exists yet, and ErrAmbiguousTenantContext if the started rows disagree.
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

// withRLS runs fn inside a transaction with app.tenant_id set via
// set_config, so the tenant_isolation_policy RLS policy on
// workflow_history_events (migration 000002) actually scopes every
// statement fn issues. Mirrors the withRLS pattern used throughout the
// platform (e.g. employee-master-svc).
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
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

	err := s.withRLS(ctx, e.TenantID, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, q,
			e.EventID,
			e.WorkflowInstanceID,
			e.EventType,
			e.CorrelationID,
			e.TenantID,
			e.LegalEntityID,
			e.Payload,
		)
		return execErr
	})
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

// ListByInstance returns all events for the given workflowInstanceID scoped
// to tenantID, ordered chronologically by recorded_at ASC.
func (s *PgStore) ListByInstance(ctx context.Context, tenantID, workflowInstanceID string) ([]WorkflowHistoryEvent, error) {
	const q = `
SELECT event_id, workflow_instance_id, event_type, correlation_id,
       tenant_id, legal_entity_id, payload, recorded_at
FROM workflow_history_events
WHERE workflow_instance_id = $1 AND tenant_id = $2
ORDER BY recorded_at ASC, event_id ASC`

	var events []WorkflowHistoryEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, q, workflowInstanceID, tenantID)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		var scanErr error
		events, scanErr = scanRows(rows)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow history by instance %q: %w", workflowInstanceID, err)
	}
	return events, nil
}

// cursorArgs splits a cursor into the two nullable query arguments of the keyset
// predicate.
func cursorArgs(after *PageCursor) (*time.Time, *string) {
	if after == nil {
		return nil, nil
	}
	t, id := after.RecordedAt, after.EventID
	return &t, &id
}

// ListByInstancePage returns one keyset page of an instance's events. The
// predicate (recorded_at, event_id) > (cursor) is evaluated against the same
// total order as ORDER BY, so pages never overlap or skip rows, including rows
// that share a recorded_at.
func (s *PgStore) ListByInstancePage(ctx context.Context, tenantID, workflowInstanceID string, after *PageCursor, limit int) ([]WorkflowHistoryEvent, error) {
	const q = `
SELECT event_id, workflow_instance_id, event_type, correlation_id,
       tenant_id, legal_entity_id, payload, recorded_at
FROM workflow_history_events
WHERE workflow_instance_id = $1 AND tenant_id = $2
  AND ($3::timestamptz IS NULL OR (recorded_at, event_id) > ($3::timestamptz, $4::text))
ORDER BY recorded_at ASC, event_id ASC
LIMIT $5`

	afterAt, afterID := cursorArgs(after)
	var events []WorkflowHistoryEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, q, workflowInstanceID, tenantID, afterAt, afterID, limit)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		var scanErr error
		events, scanErr = scanRows(rows)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow history page by instance %q: %w", workflowInstanceID, err)
	}
	return events, nil
}

// ListByFilterPage returns one keyset page of the cross-workflow window query.
func (s *PgStore) ListByFilterPage(ctx context.Context, f QueryFilter, after *PageCursor, limit int) ([]WorkflowHistoryEvent, error) {
	const q = `
SELECT event_id, workflow_instance_id, event_type, correlation_id,
       tenant_id, legal_entity_id, payload, recorded_at
FROM workflow_history_events
WHERE tenant_id = $1
  AND legal_entity_id = $2
  AND recorded_at >= $3
  AND recorded_at <= $4
  AND ($5::timestamptz IS NULL OR (recorded_at, event_id) > ($5::timestamptz, $6::text))
ORDER BY recorded_at ASC, event_id ASC
LIMIT $7`

	afterAt, afterID := cursorArgs(after)
	var events []WorkflowHistoryEvent
	err := s.withRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, q, f.TenantID, f.LegalEntityID, f.From, f.To, afterAt, afterID, limit)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		var scanErr error
		events, scanErr = scanRows(rows)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow history page by filter: %w", err)
	}
	return events, nil
}

// ListByFilter returns events matching the given filter ordered by recorded_at ASC, event_id ASC.
func (s *PgStore) ListByFilter(ctx context.Context, f QueryFilter) ([]WorkflowHistoryEvent, error) {
	const q = `
SELECT event_id, workflow_instance_id, event_type, correlation_id,
       tenant_id, legal_entity_id, payload, recorded_at
FROM workflow_history_events
WHERE tenant_id = $1
  AND legal_entity_id = $2
  AND recorded_at >= $3
  AND recorded_at <= $4
ORDER BY recorded_at ASC, event_id ASC`

	var events []WorkflowHistoryEvent
	err := s.withRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, q, f.TenantID, f.LegalEntityID, f.From, f.To)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		var scanErr error
		events, scanErr = scanRows(rows)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow history by filter: %w", err)
	}
	return events, nil
}

// GetTenantContext retrieves the tenant_id and legal_entity_id from the
// workflow.started row for the given workflow instance — the contract's
// authoritative source — and from nothing else. Returns (zero, false, nil) if
// no workflow.started row exists yet, and ErrAmbiguousTenantContext if started
// rows for the same instance id carry different tenant or legal entity values.
//
// The lookup necessarily runs without a tenant (the tenant is the value being
// looked up), so it sets the internal app.bypass_rls flag for this one
// read-only transaction. It is called only by the Kafka consumer and never
// from an HTTP-reachable path.
func (s *PgStore) GetTenantContext(ctx context.Context, workflowInstanceID string) (TenantContext, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TenantContext{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.bypass_rls', 'on', true)"); err != nil {
		return TenantContext{}, false, fmt.Errorf("set bypass_rls context: %w", err)
	}

	// LIMIT 2 is enough to tell "one context" from "conflicting contexts".
	const q = `
SELECT DISTINCT tenant_id, legal_entity_id
FROM workflow_history_events
WHERE workflow_instance_id = $1 AND event_type = $2
LIMIT 2`

	rows, err := tx.Query(ctx, q, workflowInstanceID, EventTypeWorkflowStarted)
	if err != nil {
		return TenantContext{}, false, fmt.Errorf("get tenant context for instance %q: %w", workflowInstanceID, err)
	}
	var found []TenantContext
	for rows.Next() {
		var tc TenantContext
		if err := rows.Scan(&tc.TenantID, &tc.LegalEntityID); err != nil {
			rows.Close()
			return TenantContext{}, false, fmt.Errorf("scan tenant context for instance %q: %w", workflowInstanceID, err)
		}
		found = append(found, tc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TenantContext{}, false, fmt.Errorf("get tenant context for instance %q: %w", workflowInstanceID, err)
	}

	switch len(found) {
	case 0:
		return TenantContext{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return TenantContext{}, false, fmt.Errorf("instance %q: %w", workflowInstanceID, ErrAmbiguousTenantContext)
	}
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

// SortEvents orders events as the Postgres queries do: recorded_at ASC, then
// event_id ASC. Exported so test doubles outside this package can mirror the
// real store's total order.
func SortEvents(events []WorkflowHistoryEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].RecordedAt.Equal(events[j].RecordedAt) {
			return events[i].RecordedAt.Before(events[j].RecordedAt)
		}
		return events[i].EventID < events[j].EventID
	})
}

// AfterCursor reports whether e sorts strictly after the cursor in the store's
// total order. A nil cursor admits everything.
func AfterCursor(e WorkflowHistoryEvent, after *PageCursor) bool {
	if after == nil {
		return true
	}
	if !e.RecordedAt.Equal(after.RecordedAt) {
		return e.RecordedAt.After(after.RecordedAt)
	}
	return e.EventID > after.EventID
}

// ListByInstance returns all events for the given workflowInstanceID scoped
// to tenantID, ordered like the Postgres query (RecordedAt ASC, EventID ASC).
func (f *FakeStore) ListByInstance(_ context.Context, tenantID, workflowInstanceID string) ([]WorkflowHistoryEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []WorkflowHistoryEvent
	for _, e := range f.events {
		if e.WorkflowInstanceID == workflowInstanceID && e.TenantID == tenantID {
			out = append(out, e)
		}
	}
	SortEvents(out)
	return out, nil
}

// ListByInstancePage mirrors PgStore.ListByInstancePage.
func (f *FakeStore) ListByInstancePage(ctx context.Context, tenantID, workflowInstanceID string, after *PageCursor, limit int) ([]WorkflowHistoryEvent, error) {
	all, _ := f.ListByInstance(ctx, tenantID, workflowInstanceID)
	return pageOf(all, after, limit), nil
}

func pageOf(sorted []WorkflowHistoryEvent, after *PageCursor, limit int) []WorkflowHistoryEvent {
	var out []WorkflowHistoryEvent
	for _, e := range sorted {
		if AfterCursor(e, after) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

// ListByFilterPage mirrors PgStore.ListByFilterPage.
func (f *FakeStore) ListByFilterPage(ctx context.Context, filter QueryFilter, after *PageCursor, limit int) ([]WorkflowHistoryEvent, error) {
	all, _ := f.ListByFilter(ctx, filter)
	return pageOf(all, after, limit), nil
}

// ListByFilter returns events matching the given filter, ordered by RecordedAt ASC, EventID ASC.
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
	SortEvents(out)
	return out, nil
}

// GetTenantContext mirrors PgStore: only workflow.started rows count, and
// conflicting started rows are ErrAmbiguousTenantContext.
func (f *FakeStore) GetTenantContext(_ context.Context, workflowInstanceID string) (TenantContext, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []TenantContext
	for _, e := range f.events {
		if e.WorkflowInstanceID != workflowInstanceID || e.EventType != EventTypeWorkflowStarted {
			continue
		}
		tc := TenantContext{TenantID: e.TenantID, LegalEntityID: e.LegalEntityID}
		dup := false
		for _, x := range found {
			if x == tc {
				dup = true
			}
		}
		if !dup {
			found = append(found, tc)
		}
	}
	switch len(found) {
	case 0:
		return TenantContext{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return TenantContext{}, false, fmt.Errorf("instance %q: %w", workflowInstanceID, ErrAmbiguousTenantContext)
	}
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
