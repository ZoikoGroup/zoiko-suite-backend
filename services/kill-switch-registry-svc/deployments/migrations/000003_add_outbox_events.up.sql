-- 000003_add_outbox_events.up.sql
-- Transactional Outbox Pattern for kill-switch-registry-svc
--
-- Guarantees at-least-once delivery of kill switch events (kill_switch.engaged,
-- kill_switch.disengaged) even when Kafka brokers are temporarily unavailable.
-- Handlers insert outbox records within the same database transaction as the
-- kill_switch_events record.

CREATE TABLE IF NOT EXISTS outbox_events (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type     VARCHAR(64) NOT NULL,
    aggregate_id       VARCHAR(64) NOT NULL,
    event_type         VARCHAR(128) NOT NULL,
    payload            JSONB       NOT NULL,
    correlation_id     VARCHAR(128),
    tenant_id          VARCHAR(64), -- NULL for platform-wide kill switches
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,
    publish_attempts   INT         NOT NULL DEFAULT 0,
    last_error         TEXT
);

-- Index for the background relay worker to quickly find unpublished events in FIFO order
CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;

-- Optional lookup index by tenant
CREATE INDEX IF NOT EXISTS idx_outbox_events_tenant_id
    ON outbox_events (tenant_id);
