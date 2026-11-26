-- Transactional outbox for spend.threshold.breached and spend.block.applied.
--
-- The two events used to be written straight to Kafka by the handler AFTER
-- EvaluateSpend had committed, with the write error logged and dropped: a
-- broker hiccup lost the only notice of a refused spend while the BLOCKED
-- consumption row said it had happened. They are now INSERTed here inside the
-- SAME transaction that records the BLOCKED consumption, and
-- internal/outbox.Relay delivers them with retry (at-least-once), marking each
-- row published. Same table and relay as purchase-request-svc (000005 +
-- 000008).
--
-- tenant_id / legal_entity_id are VARCHAR(255) like this service's other
-- tables. ENABLE + FORCE RLS: request-path inserts are confined to the tenant
-- set by withRLS; the relay drains every tenant's backlog and declares
-- app.outbox_relay = 'true' in its own transaction, admitted by an explicit
-- disjunct (the platform outbox standard). NULLIF: a pooled connection keeps a
-- custom GUC as '' after a transaction-local set_config resets.

CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        VARCHAR(255) NOT NULL,
    legal_entity_id  VARCHAR(255) NOT NULL,
    actor_id         VARCHAR(255),
    correlation_id   VARCHAR(255),
    -- At-most-once guard for facts that must never be emitted twice on replay.
    dedupe_key       TEXT,
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT
);
CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events (created_at, outbox_event_id) WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_outbox_events_tenant ON outbox_events (tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_outbox_events_dedupe ON outbox_events (tenant_id, event_type, dedupe_key) WHERE dedupe_key IS NOT NULL;

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
