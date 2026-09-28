-- Migration: 000007_add_outbox_events.up.sql
--
-- Transactional outbox table for accounts-payable-svc per ZS-STATE-001 Invariant I-13.
-- Decouples domain state commits from asynchronous Kafka publication.
--
-- CRITICAL INFRASTRUCTURE POSTURE:
-- This table MUST NOT enable Row-Level Security (RLS) and MUST NOT apply tenant_isolation_policy.
-- The outbox relay is a background cross-tenant infrastructure worker running on a pooled
-- connection without HTTP request tenant context (app.tenant_id is unset/NULL).
-- Enabling RLS would starve the relay and cause it to see zero rows.
-- (Conforms to accepted outbox implementations in workflow-svc and commercial-account-svc).

CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id  UUID PRIMARY KEY,
    aggregate_type   VARCHAR(64) NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        UUID NOT NULL,
    legal_entity_id  UUID,
    actor_id         VARCHAR(255),
    correlation_id   VARCHAR(255) NOT NULL,
    headers          JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT
);

-- Partial index for high-throughput relay polling of unpublished events
CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;

-- Tenant audit index for operational and forensic lookups
CREATE INDEX IF NOT EXISTS idx_outbox_events_tenant
    ON outbox_events (tenant_id, created_at DESC);
