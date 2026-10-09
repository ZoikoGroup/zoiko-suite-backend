-- Migration: 000009_add_outbox.up.sql
--
-- Transactional outbox for reliable event publishing (Event Catalogue §6,
-- GCP §2 invariant #10). Events are written to this table in the same
-- transaction as the business write, then published asynchronously by a
-- background worker. This ensures at-least-once delivery even if the
-- broker is temporarily unavailable or the request context is cancelled.

CREATE TABLE outbox (
    outbox_id      BIGSERIAL    PRIMARY KEY,
    event_type     VARCHAR(128) NOT NULL,
    payload        JSONB        NOT NULL,
    tenant_id      VARCHAR(64)  NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    actor_id       VARCHAR(64)  NOT NULL,
    correlation_id VARCHAR(64)  NOT NULL,
    idempotency_key VARCHAR(256) NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at   TIMESTAMPTZ,
    attempts       INT          NOT NULL DEFAULT 0,
    last_error     TEXT
);

CREATE INDEX idx_outbox_unpublished
    ON outbox (created_at)
    WHERE published_at IS NULL;

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;

ALTER TABLE outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON outbox
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));