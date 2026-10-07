-- Migration: 000006_add_outbox_events.up.sql
--
-- ZS-STATE-001 Invariant I-13: Transactional Outbox for accounts-receivable-svc.
-- Guarantees atomic commitment of customer invoice state mutations and their corresponding
-- domain event representations within the same database transaction.
--
-- Note on Row-Level Security:
-- outbox_events intentionally does NOT have RLS enabled and does NOT have a
-- tenant_isolation_policy. The outbox relay is an internal cross-tenant infrastructure
-- worker that polls unpublished rows across all tenants in the background without
-- an HTTP session tenant context.

CREATE TABLE outbox_events (
    outbox_event_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type      VARCHAR(64) NOT NULL,
    aggregate_id        VARCHAR(255) NOT NULL,
    event_type          VARCHAR(128) NOT NULL,
    tenant_id           UUID NOT NULL,
    legal_entity_id     UUID NOT NULL,
    actor_id            VARCHAR(255),
    correlation_id      VARCHAR(255) NOT NULL,
    headers             JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload             JSONB NOT NULL,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    published_at        TIMESTAMP WITH TIME ZONE,
    publish_attempts    INTEGER NOT NULL DEFAULT 0,
    last_error          TEXT
);

-- Partial index for fast polling of unpublished events by background relay
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;

-- Index for aggregate trace and audit queries
CREATE INDEX idx_outbox_events_aggregate ON outbox_events (tenant_id, aggregate_type, aggregate_id);
