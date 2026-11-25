-- Transactional outbox (platform pattern: purchase-request-svc 000005 + 000008).
--
-- Rows are inserted in the SAME transaction as the state change they describe
-- and drained to Kafka by internal/outbox.Relay, replacing the previous
-- "publish after commit, ignore the error" call that silently lost events
-- whenever the broker was unavailable.
--
-- tenant_id is VARCHAR like every other table in this service. RLS is ENABLED
-- and FORCED: request-path inserts run inside the command's tenant
-- transaction and are confined to that tenant. The relay is not a request: it
-- drains every tenant's backlog, so it sets app.outbox_relay = 'true' and is
-- admitted by an explicit, auditable disjunct (the platform standard). NULLIF:
-- a pooled connection keeps a custom GUC as '' after a transaction-local
-- set_config resets, so '' must mean "not set".

CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        VARCHAR(64)  NOT NULL,
    legal_entity_id  VARCHAR(64),
    actor_id         VARCHAR(255),
    correlation_id   VARCHAR(255),
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT
);
CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events (created_at, outbox_event_id) WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_outbox_events_tenant ON outbox_events (tenant_id);

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
