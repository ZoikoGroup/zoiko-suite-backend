-- 000011_add_outbox_events.up.sql
-- Workflow & Approvals Service — Transactional Outbox per ZS-STATE-001 Invariant I-13 and doc7 item 32.
--
-- Guarantees atomic domain mutation + transition history + outbox record inside
-- the same database transaction, decoupling durable state changes from Kafka availability.

CREATE TABLE outbox_events (
    outbox_event_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type       VARCHAR(64) NOT NULL,
    aggregate_id         VARCHAR(128) NOT NULL,
    event_type           VARCHAR(128) NOT NULL,
    tenant_id            UUID        NOT NULL,
    legal_entity_id      UUID        NOT NULL,
    actor_id             TEXT,
    correlation_id       TEXT,
    headers              JSONB,
    payload              JSONB       NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at         TIMESTAMPTZ,
    publish_attempts     INTEGER     NOT NULL DEFAULT 0,
    last_error           TEXT
);

-- Partial index for fast polling of unpublished records by the outbox relay
CREATE INDEX idx_outbox_events_unpublished
    ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;

-- Tenant & legal entity scoping index per platform doctrine
CREATE INDEX idx_outbox_events_tenant_entity
    ON outbox_events (tenant_id, legal_entity_id);
