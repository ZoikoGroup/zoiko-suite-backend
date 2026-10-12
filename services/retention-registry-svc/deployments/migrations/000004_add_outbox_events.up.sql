-- 000004_add_outbox_events.up.sql
-- Transactional Outbox Pattern for retention-registry-svc
--
-- Guarantees at-least-once delivery of retention and legal hold events:
--   retention_policy.created
--   legal_hold.engaged
--   legal_hold.released
-- Handlers insert outbox records within the same database transaction as the
-- domain state change.

CREATE TABLE IF NOT EXISTS outbox_events (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type     VARCHAR(64) NOT NULL,
    aggregate_id       VARCHAR(64) NOT NULL,
    event_type         VARCHAR(128) NOT NULL,
    payload            JSONB       NOT NULL,
    correlation_id     VARCHAR(128),
    tenant_id          VARCHAR(64), -- NULL for platform-wide retention policies or holds
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
