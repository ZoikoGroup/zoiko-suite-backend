-- Migration: 000004_outbox_idempotency.up.sql
--
-- Phase-0 plumbing shared by every AP service (spec §16/§17):
--   * outbox_events      — transactional outbox. Rows are inserted in the same
--                          transaction as the state change they describe and
--                          drained to Kafka by internal/outbox.Relay.
--   * idempotency_keys   — stored result of a write keyed by the caller's
--                          Idempotency-Key, so a replay returns the original
--                          status/body (internal/idempotency).
--
-- outbox_events carries no RLS: like payment-authorization-svc's it is an
-- infrastructure queue the relay drains across tenants. Every INSERT names its
-- tenant explicitly (NOT NULL).

CREATE TABLE outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        UUID NOT NULL,
    legal_entity_id  UUID NOT NULL,
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
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at, outbox_event_id) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_events_tenant ON outbox_events (tenant_id);
CREATE UNIQUE INDEX uq_outbox_events_dedupe ON outbox_events (tenant_id, event_type, dedupe_key) WHERE dedupe_key IS NOT NULL;

CREATE TABLE idempotency_keys (
    tenant_id        UUID NOT NULL,
    idempotency_key  TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    method           TEXT NOT NULL,
    path             TEXT NOT NULL,
    -- NULL status_code = request still in flight (or abandoned).
    status_code      INT,
    response_body    BYTEA,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON idempotency_keys
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
