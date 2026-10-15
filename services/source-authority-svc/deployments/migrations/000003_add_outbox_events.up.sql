-- 000003_add_outbox_events.up.sql
-- Transactional Outbox Pattern for source-authority-svc
--
-- Guarantees at-least-once delivery of:
--   source_authority_map.created
--   source_authority_map.superseded
--   normalized_fact.recorded
-- Handlers insert outbox records within the same database transaction as the
-- domain state change, replacing the direct, error-swallowed
-- `_ = h.publisher.Publish(...)` calls that previously ran after commit with
-- no retry on a Kafka outage.

CREATE TABLE IF NOT EXISTS outbox_events (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type     VARCHAR(64) NOT NULL,
    aggregate_id       VARCHAR(64) NOT NULL,
    event_type         VARCHAR(128) NOT NULL,
    payload            JSONB       NOT NULL,
    correlation_id     VARCHAR(128),
    -- NULL for source_authority_map events (platform-wide reference data,
    -- same posture as metric-registry-svc), populated for normalized_fact
    -- events (tenant business data).
    tenant_id          VARCHAR(64),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,
    publish_attempts   INT         NOT NULL DEFAULT 0,
    last_error         TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_outbox_events_tenant_id
    ON outbox_events (tenant_id);
