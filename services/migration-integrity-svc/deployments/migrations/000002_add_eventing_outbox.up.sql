-- Migration: 000002_add_eventing_outbox.up.sql
--
-- Transactional event outbox (ZS-EVENT-001 §6) — the dual-write fix.
-- Every authoritative producer of a material event must make the state change
-- and the corresponding outbox record part of one local atomic transaction.
-- The table and indexes are services/eventing/outbox/schema.go:SchemaSQL
-- verbatim, which VerifySchema() checks at startup. Do not modify column
-- names, types, or indexes — the shared relay depends on this exact schema.

CREATE TABLE eventing_outbox (
    outbox_id           UUID         PRIMARY KEY,
    event_id            TEXT         NOT NULL UNIQUE,
    event_type          TEXT         NOT NULL,
    schema_version      TEXT         NOT NULL,
    tenant_id           TEXT         NOT NULL,
    region              TEXT         NOT NULL,
    aggregate_id        TEXT,
    aggregate_version   BIGINT,
    partition_key       TEXT         NOT NULL,
    payload             BYTEA        NOT NULL,
    payload_hash        TEXT         NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    publish_state       TEXT         NOT NULL DEFAULT 'pending',
    attempt_count       INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    claimed_until       TIMESTAMPTZ,
    dispatched_at       TIMESTAMPTZ,
    published_at        TIMESTAMPTZ,
    last_error_code     TEXT,
    last_error          TEXT,

    CONSTRAINT eventing_outbox_state_valid CHECK (publish_state IN
        ('pending', 'claimed', 'published', 'failed', 'quarantined')),
    CONSTRAINT eventing_outbox_attempts_nonneg CHECK (attempt_count >= 0),
    CONSTRAINT eventing_outbox_published_has_time CHECK
        (publish_state <> 'published' OR published_at IS NOT NULL),
    CONSTRAINT eventing_outbox_claimed_has_lease CHECK
        (publish_state <> 'claimed' OR claimed_until IS NOT NULL)
);

-- Everything not yet delivered, oldest first. Serves the dispatcher's claim
-- scan and the backlog signals; published rows (which accumulate until
-- retention removes them) are outside it, so neither query slows as history
-- grows.
CREATE INDEX eventing_outbox_backlog_idx ON eventing_outbox (created_at)
    WHERE publish_state IN ('pending', 'failed', 'claimed');
CREATE INDEX eventing_outbox_quarantined_idx ON eventing_outbox (created_at)
    WHERE publish_state = 'quarantined';

-- Tenant isolation, with one explicit escape hatch for the relay.
--
-- Request-path writes install app.tenant_id and are confined to it. The relay
-- drains every tenant's backlog from one background loop and installs
-- app.outbox_relay instead (eventing/outbox inRelayTx). A tenant-only policy
-- would hide every row from it under FORCE ROW LEVEL SECURITY for any role
-- without BYPASSRLS — zoiko_app included — and present as a relay that never
-- publishes anything. NULLIF because a transaction-local custom GUC reads as
-- '' (not NULL) on a pooled connection that has already served a request.
ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
