-- Migration: 000008_add_command_idempotency.up.sql
--
-- SPEC_DEVIATIONS A14. ACC-12/13's commands had no Idempotency-Key: a retried
-- POST /v1/consolidation/runs created a second run, and the four adjustment
-- commands (create, approve, post, reverse) relied on their status guards
-- alone, so a retry after a lost response got 422 instead of its result.
--
-- This is the platform's command_idempotency table and rule, the one
-- financial-control-svc's BeginExecution uses (internal/store/execution.go)
-- and general-ledger-svc follows for ACC-01: the key is unique per tenant,
-- names the command it was used for and the one resource it acted on, and is
-- recorded in the same transaction as the write it protects. A repeat for the
-- same command and resource is a replay; the same key for anything else is a
-- conflict.
--
-- Two target columns rather than one generic id, so each target is a real
-- foreign key: a run (StartRun) or a consolidation adjustment (create,
-- approve, post, reverse). Exactly one is set — a claim that names both or
-- neither cannot say what it protects.
CREATE TABLE command_idempotency (
    tenant_id                   VARCHAR(255) NOT NULL,
    idempotency_key             VARCHAR(255) NOT NULL,
    command                     VARCHAR(64)  NOT NULL,
    consolidation_run_id        UUID REFERENCES consolidation_runs(consolidation_run_id),
    consolidation_adjustment_id UUID REFERENCES consolidation_adjustments(consolidation_adjustment_id),
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, idempotency_key),
    CONSTRAINT chk_command_idempotency_one_target
        CHECK (num_nonnulls(consolidation_run_id, consolidation_adjustment_id) = 1)
);

-- The PK answers the only lookup the command path makes: by (tenant, key).
-- These two exist for the audit question "which keys acted on this run or
-- adjustment", and keep the foreign keys cheap to check.
CREATE INDEX idx_command_idempotency_run ON command_idempotency (tenant_id, consolidation_run_id);
CREATE INDEX idx_command_idempotency_adjustment ON command_idempotency (tenant_id, consolidation_adjustment_id);

ALTER TABLE command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON command_idempotency
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
