-- Migration: 000006_outbox_events.up.sql
--
-- Adds transactional outbox table for reliable event publishing.
-- Events are written in the same DB transaction as the domain change,
-- then a background relay publishes them to Kafka.

CREATE TABLE outbox_events (
    outbox_event_id      UUID PRIMARY KEY,
    aggregate_type       VARCHAR(64) NOT NULL,
    aggregate_id         UUID NOT NULL,
    event_type           VARCHAR(64) NOT NULL,
    payload              JSONB NOT NULL,
    headers              JSONB NOT NULL DEFAULT '{}'::jsonb,
    correlation_id       TEXT,
    tenant_id            UUID NOT NULL,
    legal_entity_id      UUID NOT NULL,
    actor_id             TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at         TIMESTAMPTZ,
    publish_attempts     INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT
);

CREATE INDEX idx_outbox_unpublished ON outbox_events (published_at) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_aggregate ON outbox_events (aggregate_type, aggregate_id);
CREATE INDEX idx_outbox_tenant ON outbox_events (tenant_id);

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON outbox_events
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;