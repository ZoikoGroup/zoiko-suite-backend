-- Transactional outbox.
--
-- Domain events used to be published to Kafka directly after the business
-- transaction committed, with the publish error ignored: a broker hiccup lost
-- the only notice of the change. Events are now written to outbox_events in
-- the SAME transaction as the change (internal/outbox.Insert) and delivered by
-- internal/outbox.Relay with retry; a row is marked published only after Kafka
-- accepted it.
--
-- tenant_id is TEXT, like every other tenant column in this service.
--
-- ENABLE + FORCE ROW LEVEL SECURITY: request-path inserts run inside the
-- command's tenant transaction and are confined to that tenant. The relay is
-- not a request: it drains every tenant's backlog, so it sets
-- app.outbox_relay = 'true' in its own transaction and is admitted by an
-- explicit, auditable disjunct (platform pattern: purchase-request-svc
-- 000008_outbox_rls, compliance-risk-scoring-svc 000004_outbox_rls).
-- NULLIF: a pooled connection keeps a custom GUC as '' after a
-- transaction-local set_config resets, so '' must mean "not set".
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id  UUID PRIMARY KEY,
    aggregate_type   TEXT        NOT NULL,
    aggregate_id     TEXT        NOT NULL,
    event_type       TEXT        NOT NULL,
    tenant_id        TEXT        NOT NULL,
    legal_entity_id  TEXT,
    actor_id         TEXT,
    correlation_id   TEXT,
    dedupe_key       TEXT,
    payload          JSONB       NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT         NOT NULL DEFAULT 0,
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
