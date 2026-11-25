-- Transactional outbox.
--
-- Events used to be published straight to Kafka after the business
-- transaction committed, with the error ignored: a broker outage silently
-- lost the fact. Each command now writes its event here in the SAME
-- transaction as the state change (internal/outbox.Insert), and
-- internal/outbox.Relay drains unpublished rows to Kafka with retry.
--
-- ENABLE + FORCE ROW LEVEL SECURITY like every other tenant table.
-- Request-path inserts run inside the command's tenant transaction and are
-- confined to that tenant. The relay is not a request: it drains every
-- tenant's backlog, so it sets app.outbox_relay = 'true' in its own
-- transaction and is admitted by an explicit, auditable disjunct (platform
-- pattern, e.g. purchase-request-svc 000008, compliance-risk-scoring-svc 000004).
-- NULLIF: a pooled connection keeps a custom GUC as '' after a
-- transaction-local set_config resets, so '' must mean "not set".

CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id  UUID         PRIMARY KEY,
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        TEXT         NOT NULL,
    legal_entity_id  TEXT         NOT NULL DEFAULT '',
    actor_id         VARCHAR(255),
    correlation_id   VARCHAR(255),
    payload          JSONB        NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT          NOT NULL DEFAULT 0,
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
