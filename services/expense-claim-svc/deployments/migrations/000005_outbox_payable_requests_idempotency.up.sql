-- AP-07 hardening, part 2: transactional outbox, durable AP-08 hand-off
-- records, idempotency keys, and strict (no NULL-tenant) row-level security.

-- Transactional outbox. Rows are inserted in the same transaction as the
-- state change; internal/outbox drains them to Kafka. Infrastructure queue
-- polled across tenants by the relay, so no RLS (matches the other outbox
-- tables in this repo); not covered by an append-only trigger because the
-- relay must stamp published_at / attempts.
CREATE TABLE outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
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
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_events_tenant ON outbox_events (tenant_id);

-- Durable AP-08 hand-off. Written in the approval transaction; the payable
-- relay drains it (idempotently, source_reference = claim_id) and records
-- the payable id. A claim with no controlled reimbursement payee sits in
-- BLOCKED with a stable reason; it is never paid to a fallback identity.
CREATE TABLE payable_requests (
    request_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL,
    legal_entity_id         UUID NOT NULL,
    claim_id                UUID NOT NULL UNIQUE REFERENCES expense_claims (claim_id),
    claimant_principal_id   TEXT NOT NULL,
    payment_preference_ref  TEXT NOT NULL DEFAULT '',
    requested_by            TEXT NOT NULL,
    correlation_id          TEXT NOT NULL DEFAULT '',
    amount                  NUMERIC(18, 2) NOT NULL CHECK (amount > 0),
    currency                TEXT NOT NULL,
    due_date                TIMESTAMPTZ NOT NULL,
    state                   TEXT NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'BLOCKED', 'CREATED')),
    blocked_reason          TEXT NOT NULL DEFAULT '',
    attempts                INT  NOT NULL DEFAULT 0,
    last_error              TEXT NOT NULL DEFAULT '',
    next_attempt_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    payable_id              TEXT NOT NULL DEFAULT '',
    payee_ref               TEXT NOT NULL DEFAULT '',
    payee_destination_id    TEXT NOT NULL DEFAULT '',
    settlement_checked_at   TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_payable_requests_due ON payable_requests (next_attempt_at) WHERE state IN ('PENDING', 'BLOCKED');

CREATE OR REPLACE FUNCTION reject_payable_request_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payable_requests rows are never deleted';
    END IF;
    -- Once CREATED the record is complete; only the relay's settlement-poll
    -- bookkeeping column may still move.
    IF OLD.state = 'CREATED' AND (to_jsonb(NEW) - 'settlement_checked_at') IS DISTINCT FROM (to_jsonb(OLD) - 'settlement_checked_at') THEN
        RAISE EXCEPTION 'payable request % is complete and cannot be modified', OLD.request_id;
    END IF;
    IF NEW.claim_id IS DISTINCT FROM OLD.claim_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.due_date IS DISTINCT FROM OLD.due_date
        OR NEW.claimant_principal_id IS DISTINCT FROM OLD.claimant_principal_id
    THEN
        RAISE EXCEPTION 'payable request % basis can never change', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_payable_request_mutation
    BEFORE UPDATE OR DELETE ON payable_requests
    FOR EACH ROW EXECUTE FUNCTION reject_payable_request_mutation();

-- Idempotency: the stored result of a command, keyed by the caller's
-- Idempotency-Key within its tenant. Written in the command's own
-- transaction, so a committed command always has its replay record.
CREATE TABLE expense_claim_idempotency (
    tenant_id        UUID NOT NULL,
    idempotency_key  TEXT NOT NULL,
    operation        TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    status_code      INT  NOT NULL,
    response         JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);
CREATE TRIGGER trg_reject_expense_claim_idempotency_mutation
    BEFORE UPDATE OR DELETE ON expense_claim_idempotency
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

-- Strict tenant isolation: no "tenant_id IS NULL" escape hatch. The payable
-- relay is not a request; it installs app.system_relay and is admitted by
-- the second disjunct (payable_requests only).
ALTER TABLE payable_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE payable_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payable_requests
    USING (
        tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.system_relay', true), ''), 'false') = 'true'
    );

ALTER TABLE expense_claim_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE expense_claim_idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON expense_claim_idempotency
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- The settlement poll (REIMBURSABLE -> CLOSED) also runs outside a request.
DROP POLICY tenant_isolation ON expense_claims;
CREATE POLICY tenant_isolation ON expense_claims
    USING (
        tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.system_relay', true), ''), 'false') = 'true'
    );

DROP POLICY tenant_isolation ON expense_lines;
CREATE POLICY tenant_isolation ON expense_lines
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY tenant_isolation ON expense_claim_events;
CREATE POLICY tenant_isolation ON expense_claim_events
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
