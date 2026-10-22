-- Migration: 000009_command_idempotency.up.sql
--
-- SPEC_DEVIATIONS A10. ACC-11's lifecycle commands (AcknowledgeCounterparty,
-- MatchIntercompany, DisputeIntercompany, ResolveMismatch) had no
-- Idempotency-Key: a retried dispute after a lost response got 422 instead of
-- replaying its original result. Create is already idempotent by
-- (tenant_id, source_journal_id) and stays that way.
--
-- This is the platform's command_idempotency table and rule, the one
-- financial-control-svc's BeginExecution uses (internal/store/execution.go)
-- and consolidation-svc follows for ACC-12/13: the key is unique per tenant,
-- names the command it was used for and the one entry it acted on, and is
-- recorded in the same transaction as the write it protects. A repeat for the
-- same command and entry is a replay; the same key for anything else is a
-- conflict.

CREATE TABLE command_idempotency (
    tenant_id               VARCHAR(255) NOT NULL,
    idempotency_key         VARCHAR(255) NOT NULL,
    command                 VARCHAR(64)  NOT NULL,
    intercompany_entry_id   UUID         NOT NULL REFERENCES intercompany_entries(intercompany_entry_id),
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, idempotency_key)
);

-- The PK answers the only lookup the command path makes: by (tenant, key).
-- This index exists for the audit question "which keys acted on this entry",
-- and keeps the foreign key cheap to check.
CREATE INDEX idx_command_idempotency_entry ON command_idempotency (tenant_id, intercompany_entry_id);

ALTER TABLE command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON command_idempotency
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
