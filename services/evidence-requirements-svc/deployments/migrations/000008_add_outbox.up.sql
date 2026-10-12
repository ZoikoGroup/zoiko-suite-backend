-- Migration: 000008_add_outbox.up.sql
-- Transactional outbox for reliable event publishing
-- Per Event Catalogue §6: dual write to DB and broker without outbox is prohibited

CREATE TABLE outbox (
    outbox_id          UUID PRIMARY KEY,
    aggregate_type     VARCHAR(64) NOT NULL,
    aggregate_id       VARCHAR(64) NOT NULL,
    event_type         VARCHAR(128) NOT NULL,
    payload            JSONB NOT NULL,
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    published_at       TIMESTAMP WITH TIME ZONE,
    publish_attempts   INT NOT NULL DEFAULT 0,
    last_error         TEXT
);

CREATE INDEX idx_outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON outbox
    FOR ALL USING (false); -- Platform table, no tenant isolation

-- Function to claim unpublished events for publishing
CREATE OR REPLACE FUNCTION claim_outbox_events(batch_size INT DEFAULT 100)
RETURNS SETOF outbox AS $$
DECLARE
    claimed outbox;
BEGIN
    FOR claimed IN
        UPDATE outbox
        SET publish_attempts = publish_attempts + 1
        WHERE outbox_id IN (
            SELECT outbox_id FROM outbox
            WHERE published_at IS NULL
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT batch_size
        )
        RETURNING *
    LOOP
        RETURN NEXT claimed;
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- Function to mark events as published
CREATE OR REPLACE FUNCTION mark_outbox_published(ids UUID[])
RETURNS VOID AS $$
BEGIN
    UPDATE outbox
    SET published_at = NOW(), last_error = NULL
    WHERE outbox_id = ANY(ids);
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- Function to record publish failure
CREATE OR REPLACE FUNCTION mark_outbox_failed(ids UUID[], err TEXT)
RETURNS VOID AS $$
BEGIN
    UPDATE outbox
    SET last_error = err
    WHERE outbox_id = ANY(ids);
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;