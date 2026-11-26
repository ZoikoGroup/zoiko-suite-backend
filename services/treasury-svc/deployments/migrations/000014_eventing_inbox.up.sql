-- Migration: 000014_eventing_inbox.up.sql
--
-- The shared consumer inbox (ZS-SVC-M-001 INT-03), copied verbatim from
-- zoiko.io/eventing/inbox.SchemaSQL + RLSSQL (region column included from
-- the start — see INT-NP-045), the same adoption pattern
-- bank-reconciliation-svc used for its own first cross-service consumer
-- (see that service's 000012_eventing_inbox.up.sql /
-- 000013_inbox_region.up.sql). This is treasury-svc's first event
-- consumer: the BankStatementAccepted listener that replaces the dead
-- CreateCashBalance(0.00) placeholder with a real bank balance (BNK-08).

CREATE TABLE eventing_inbox (
    inbox_id          UUID         PRIMARY KEY,
    consumer          TEXT         NOT NULL,
    event_id          TEXT         NOT NULL,
    event_type        TEXT         NOT NULL,
    tenant_id         TEXT         NOT NULL,
    aggregate_id      TEXT,
    aggregate_version BIGINT,
    payload_hash      TEXT         NOT NULL,
    state             TEXT         NOT NULL DEFAULT 'processing',
    attempt_count     INTEGER      NOT NULL DEFAULT 1,
    duplicate_count   INTEGER      NOT NULL DEFAULT 0,
    received_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    result_ref        TEXT,
    last_error_code   TEXT,
    last_error        TEXT,
    region            TEXT,

    CONSTRAINT eventing_inbox_unique_delivery UNIQUE (consumer, tenant_id, event_id),
    CONSTRAINT eventing_inbox_state_valid CHECK (state IN
        ('received', 'processing', 'completed', 'failed_retryable', 'quarantined', 'superseded')),
    CONSTRAINT eventing_inbox_attempts_nonneg CHECK (attempt_count >= 0 AND duplicate_count >= 0),
    CONSTRAINT eventing_inbox_completed_has_time CHECK
        (state NOT IN ('completed', 'superseded') OR completed_at IS NOT NULL)
);

CREATE INDEX eventing_inbox_aggregate_idx
    ON eventing_inbox (consumer, tenant_id, aggregate_id, aggregate_version)
    WHERE state = 'completed' AND aggregate_id IS NOT NULL;

CREATE INDEX eventing_inbox_attention_idx ON eventing_inbox (updated_at)
    WHERE state IN ('failed_retryable', 'quarantined', 'processing', 'received');

ALTER TABLE eventing_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_inbox FORCE ROW LEVEL SECURITY;

CREATE POLICY eventing_inbox_tenant_isolation ON eventing_inbox FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
