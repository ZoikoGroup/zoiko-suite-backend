-- Migration: 000005_add_command_idempotency.up.sql
--
-- SPEC_DEVIATIONS A1. ORG-10's six commands had no Idempotency-Key. A retry
-- after a lost response re-ran the state transition and, in the worst case,
-- proposed a second candidate. This is the platform's command_idempotency
-- table and rule used by financial-control-svc, general-ledger-svc,
-- reconciliation-engine-svc, consolidation-svc and
-- intercompany-accounting-svc: the key is unique per tenant, names the
-- command it was used for and the one payee destination it acted on, and is
-- recorded in the same transaction as the write it protects. A repeat for
-- the same command and destination is a replay; the same key for anything
-- else is a conflict. A refused command never inserts a row, so it never
-- burns its key.
--
-- ProposeDestination has no target until it creates one; it inserts the
-- generated destination_id here after the candidate row exists, in the same
-- transaction.

CREATE TABLE command_idempotency (
    tenant_id       UUID         NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    command         VARCHAR(64)  NOT NULL,
    destination_id  UUID         NOT NULL REFERENCES payee_destinations (destination_id),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, idempotency_key)
);

-- The primary key answers the command path's only lookup, by (tenant, key).
-- This index answers the audit question "which keys acted on this
-- destination" and keeps the foreign key cheap to check.
CREATE INDEX idx_command_idempotency_destination
    ON command_idempotency (tenant_id, destination_id);

ALTER TABLE command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON command_idempotency
    FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
