-- Migration: 000003_add_outbox.up.sql
--
-- Transactional outbox for reliable event publishing. Events are written
-- to this table in the same transaction as the business write, then
-- published asynchronously by a background worker. This ensures at-least-
-- once delivery even if the broker is temporarily unavailable.

CREATE TABLE outbox (
    outbox_id          BIGSERIAL    PRIMARY KEY,
    event_type         VARCHAR(128) NOT NULL,
    event_version      VARCHAR(32)  NOT NULL DEFAULT '1.0',
    schema_version     VARCHAR(32)  NOT NULL DEFAULT '1.0',
    source_service     VARCHAR(64)  NOT NULL DEFAULT 'policy-svc',
    tenant_id          UUID,
    legal_entity_id    UUID,
    actor_id           TEXT,
    correlation_id     TEXT,
    idempotency_key    TEXT         NOT NULL,
    payload            JSONB        NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at       TIMESTAMPTZ,
    attempts           INT          NOT NULL DEFAULT 0,
    last_error         TEXT
);

CREATE INDEX idx_outbox_unpublished
    ON outbox (created_at)
    WHERE published_at IS NULL;

COMMENT ON TABLE outbox IS 'Transactional outbox for event publishing - events written here are published asynchronously';
COMMENT ON COLUMN outbox.idempotency_key IS 'Idempotency key from the request that generated this event - used for exactly-once processing';