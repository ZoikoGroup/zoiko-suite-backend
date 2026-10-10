-- Persist domain events in the same transaction as capability-registry writes.
-- The relay retries pending events and uses the stored payload/event ID unchanged.
CREATE TABLE outbox_events (
    outbox_event_id UUID PRIMARY KEY,
    entity_id       TEXT NOT NULL,
    payload         JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    publish_attempts INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    claim_token     UUID,
    claimed_until   TIMESTAMPTZ
);

CREATE INDEX idx_capability_outbox_pending
    ON outbox_events (created_at, outbox_event_id)
    WHERE published_at IS NULL;
