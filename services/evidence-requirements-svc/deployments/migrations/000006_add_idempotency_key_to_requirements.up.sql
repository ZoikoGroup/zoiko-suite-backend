-- Migration: 000006_add_idempotency_key_to_requirements.up.sql
-- Add idempotency_key column and change deduplication key from correlation_id to idempotency_key
-- Per API standard §16 INV-08: Idempotency-Key is the replay protection token

ALTER TABLE evidence_requirements
    ADD COLUMN idempotency_key VARCHAR(255) NOT NULL DEFAULT '';

-- Backfill existing rows with their correlation_id (best effort for existing data)
UPDATE evidence_requirements
SET idempotency_key = correlation_id
WHERE idempotency_key = '';

-- Make it non-nullable after backfill
ALTER TABLE evidence_requirements
    ALTER COLUMN idempotency_key SET NOT NULL;

-- Drop old unique index on (tenant_id, correlation_id)
DROP INDEX IF EXISTS idx_evidence_requirements_tenant_correlation;

-- Add new unique index on (tenant_id, idempotency_key)
CREATE UNIQUE INDEX idx_evidence_requirements_tenant_idempotency
    ON evidence_requirements (tenant_id, idempotency_key);

-- Keep correlation_id column for traceability but remove uniqueness