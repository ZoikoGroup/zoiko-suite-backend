-- 000010_event_outbox.up.sql
-- Obligations Service — transactional outbox for reliable event publishing
--
-- Platform standard D10: Events lost on broker failure. 18 of 20 services
-- publish after commit, swallow the error and have no outbox.
-- This adds an outbox table so events are persisted in the same transaction
-- as the business data, then published reliably by a background worker.

BEGIN;

CREATE TABLE event_outbox (
    outbox_id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID        NOT NULL,
    event_type        VARCHAR(128) NOT NULL,
    event_payload     JSONB       NOT NULL,
    correlation_id    TEXT        NOT NULL,
    -- Status: PENDING, PUBLISHED, FAILED
    status            VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    -- Number of publish attempts
    attempts          INTEGER     NOT NULL DEFAULT 0,
    -- Last error message if failed
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at      TIMESTAMPTZ
);

-- Index for polling unpublished events
CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox (tenant_id, status, created_at)
    WHERE status = 'PENDING';

-- RLS for event_outbox
ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON event_outbox;
CREATE POLICY tenant_isolation_policy ON event_outbox FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;