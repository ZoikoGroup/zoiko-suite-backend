-- Migration: 000005_add_idempotency_key_to_evaluations.down.sql
-- Revert idempotency_key changes

DROP INDEX IF EXISTS idx_evidence_evaluations_tenant_idempotency;

-- Restore old unique index on (tenant_id, correlation_id)
CREATE UNIQUE INDEX idx_evidence_evaluations_tenant_correlation
    ON evidence_evaluations (tenant_id, correlation_id);

ALTER TABLE evidence_evaluations
    DROP COLUMN IF EXISTS idempotency_key;