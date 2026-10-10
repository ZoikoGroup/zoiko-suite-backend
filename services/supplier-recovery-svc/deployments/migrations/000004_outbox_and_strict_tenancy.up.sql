-- 000004_outbox_and_strict_tenancy.up.sql
-- supplier-recovery-svc (AP-12):
--   * outbox_events: transactional outbox. Events are written in the same
--     transaction as the state change; a relay publishes them to Kafka.
--     It is an internal queue polled across all tenants, so it carries no RLS
--     and no append-only trigger (the relay updates published_at etc.).
--   * tightened RLS: rows are tenant-scoped; a request with no tenant sees
--     nothing (the earlier "tenant_id IS NULL" escape hatch is removed).

CREATE TABLE outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        UUID NULL,
    legal_entity_id  TEXT NOT NULL,
    actor_id         VARCHAR(255) NULL,
    correlation_id   VARCHAR(255) NULL,
    headers          JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ NULL,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT NULL
);
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_events_tenant ON outbox_events (tenant_id);

DROP POLICY IF EXISTS tenant_isolation ON supplier_recovery_cases;
CREATE POLICY tenant_isolation ON supplier_recovery_cases
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON recovery_applications;
CREATE POLICY tenant_isolation ON recovery_applications
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON recovery_commitments;
CREATE POLICY tenant_isolation ON recovery_commitments
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
