-- Migration: 000006_add_idempotency_key_to_requirements.down.sql
-- Revert idempotency_key changes

DROP INDEX IF EXISTS idx_evidence_requirements_tenant_idempotency;

-- Restore old unique index on (tenant_id, correlation_id)
CREATE UNIQUE INDEX idx_evidence_requirements_tenant_correlation
    ON evidence_requirements (tenant_id, correlation_id);

ALTER TABLE evidence_requirements
    DROP COLUMN IF EXISTS idempotency_key;