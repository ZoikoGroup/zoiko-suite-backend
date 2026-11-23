-- 000006_event_outbox.up.sql
--
-- Transactional outbox for the ORG-05 events (ZS-SVC-I-001 s4.5: "downstream
-- consumers receive change events with stable object/version identity", and
-- section 2.1: every high-risk master-data change emits an authoritative
-- event). The event row is written in the same transaction as the change it
-- attests, so the two land together or not at all; a relay delivers what landed.
--
-- Same shape as tenant-entity-registry-svc's event_outbox (000006) and
-- identity-context-svc's, so the estate has one outbox pattern. Two additions:
-- tenant_id is VARCHAR here, like every table in this service, and seq orders
-- events written in one transaction (they share a created_at).

CREATE TABLE IF NOT EXISTS event_outbox (
    seq             BIGSERIAL,
    event_id        UUID PRIMARY KEY,
    event_type      VARCHAR(128) NOT NULL,
    tenant_id       VARCHAR(255) NOT NULL,
    partition_key   VARCHAR(255) NOT NULL,

    -- The fully rendered envelope, so what is delivered is what was decided
    -- even if the envelope struct changes shape in a later release.
    payload         JSONB NOT NULL,

    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMP WITH TIME ZONE,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_error      TEXT,

    CONSTRAINT event_outbox_attempts_nonneg CHECK (attempts >= 0)
);

CREATE INDEX idx_event_outbox_pending
    ON event_outbox (next_attempt_at, created_at, seq)
    WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

-- Normal writes are tenant-scoped like every other table. The relay drains
-- every tenant's events from one process and so cannot set a single
-- app.tenant_id; it sets app.outbox_relay instead, a named capability that
-- appears in exactly one place in the code (internal/outbox) and grants access
-- to no other table.
CREATE POLICY tenant_isolation_event_outbox ON event_outbox
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
    );
