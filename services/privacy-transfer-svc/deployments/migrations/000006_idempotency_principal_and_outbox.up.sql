-- 000006_idempotency_principal_and_outbox.up.sql
-- 1. Idempotency is scoped to (tenant, principal, key): a key never replays a
--    response produced for another principal. Existing rows predate principal
--    scoping and keep principal_id = '' - they can never match a real
--    principal, so they are inert.
-- 2. Transactional outbox (§19.1): the §19 events are enqueued in the same
--    transaction as the state change they announce.

ALTER TABLE transfer_idempotency_keys
    ADD COLUMN IF NOT EXISTS principal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_idempotency_keys DROP CONSTRAINT IF EXISTS transfer_idempotency_keys_pkey;
ALTER TABLE transfer_idempotency_keys
    ADD CONSTRAINT transfer_idempotency_keys_pkey PRIMARY KEY (tenant_id, principal_id, idempotency_key);

CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_event_id VARCHAR(255) PRIMARY KEY,
    event_type      VARCHAR(200) NOT NULL,
    tenant_id       VARCHAR(255) NOT NULL DEFAULT '',
    entity_id       VARCHAR(255) NOT NULL DEFAULT '',
    actor_id        VARCHAR(255) NOT NULL DEFAULT '',
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    payload         JSONB        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    attempts        INT          NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_error      TEXT,
    CONSTRAINT event_outbox_attempts_nonneg CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON event_outbox;
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        current_setting('app.outbox_relay', true) = 'true'
        OR (tenant_id <> '' AND tenant_id = COALESCE(current_setting('app.tenant_id', true), ''))
    )
    WITH CHECK (
        current_setting('app.outbox_relay', true) = 'true'
        OR (tenant_id <> '' AND tenant_id = COALESCE(current_setting('app.tenant_id', true), ''))
    );
