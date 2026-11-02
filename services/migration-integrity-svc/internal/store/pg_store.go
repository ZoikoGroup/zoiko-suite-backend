package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

// ─── PgStore ──────────────────────────────────────────────────────────────────

const (
	aggregateJob        = "migration-job"
	aggregateAuditEntry = "migration-audit-entry"
)

// PgStore is the Postgres implementation of Store. It is the only store the
// running service uses: evidence and its outbox events must be durable.
type PgStore struct {
	pool        *pgxpool.Pool
	log         *zap.Logger
	eventRegion string
}

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (config EVENT_RESIDENCY_REGION).
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

// NewPgStore builds a PgStore over an existing pool.
func NewPgStore(pool *pgxpool.Pool, log *zap.Logger, opts ...Option) *PgStore {
	s := &PgStore{pool: pool, log: log}
	for _, o := range opts {
		o(s)
	}
	return s
}

// inTenantTx runs fn in a transaction scoped to tenantID for row-level
// security (set_config(..., true) is transaction-local, so the setting can
// never leak to the next request on a pooled connection). fn's error rolls
// the transaction back; a nil error commits it.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("install tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ─── Events ───────────────────────────────────────────────────────────────────

// migrationEvent describes one evidence event. TypeSegment and Fact build the
// canonical com.zoikosuite.migration.<segment>.<fact> (kebab-case only:
// envelope.New rejects underscores). LegacyType is the exact pre-standard
// event_type consumers filter on; it is evidence-relevant and never derived.
type migrationEvent struct {
	TypeSegment   string
	Fact          string
	LegacyType    string
	AggregateType string
	AggregateID   string
	LegalEntityID string
	Data          any
}

// enqueueEvent writes one evidence event to the transactional outbox inside
// tx, so it commits or rolls back with the change it reports.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, tenantID, actorID, correlationID string, ev migrationEvent) error {
	env, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.migration." + ev.TypeSegment + "." + ev.Fact,
		LegacyType:      ev.LegacyType,
		Service:         "migration-integrity-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   ev.LegalEntityID,
		AggregateType:   ev.AggregateType,
		AggregateID:     ev.AggregateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Confidential,
		Data:            ev.Data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", ev.LegacyType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", ev.LegacyType, err)
	}
	return nil
}

// isNoRows reports whether err is pgx's "no rows" (a missing or
// RLS-hidden row).
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
