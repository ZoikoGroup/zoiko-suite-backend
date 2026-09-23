-- Migration 000005: Add Transactional Outbox Events table for payment-authorization-svc
-- Enforces ZS-STATE-001 Invariant I-13: Domain event publication follows durable
-- authoritative state changes via Transactional Outbox.

CREATE TABLE outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64) NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        UUID NULL,
    legal_entity_id  UUID NOT NULL,
    actor_id         VARCHAR(255) NULL,
    correlation_id   VARCHAR(255) NULL,
    headers          JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ NULL,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT NULL
);

-- Partial index for high-performance retrieval of unpublished events
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC) WHERE published_at IS NULL;

-- Tenant lookup index
CREATE INDEX idx_outbox_events_tenant ON outbox_events (tenant_id);

-- IMPORTANT: outbox_events is an internal infrastructure queue polled across all tenants
-- by the background relay worker. RLS is omitted matching workflow-svc and commercial-account-svc conventions.
-- Do NOT apply reject_evidence_mutation trigger to outbox_events,
-- because the background outbox relay must update published_at, publish_attempts, and last_error.
