-- 000002_add_outbox_events.up.sql
-- Transactional Outbox Pattern for metric-registry-svc
--
-- Guarantees at-least-once delivery of:
--   report_metric.created
--   report_metric.version_published
-- Handlers insert outbox records within the same database transaction as
-- the domain state change (CreateMetricDefinition, PublishNewVersion),
-- replacing a direct Kafka publish whose error was discarded
-- (`_ = h.publisher.Publish(...)`) — on a broker outage the domain row was
-- still committed, but the event was permanently lost, with no retry and
-- no record that anything had gone missing.

CREATE TABLE IF NOT EXISTS outbox_events (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type     VARCHAR(64) NOT NULL,
    aggregate_id       VARCHAR(64) NOT NULL,
    event_type         VARCHAR(128) NOT NULL,
    payload            JSONB       NOT NULL,
    correlation_id     VARCHAR(128),
    tenant_id          VARCHAR(64), -- always NULL: report metric definitions are platform-wide, not tenant-scoped
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,
    publish_attempts   INT         NOT NULL DEFAULT 0,
    last_error         TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;
