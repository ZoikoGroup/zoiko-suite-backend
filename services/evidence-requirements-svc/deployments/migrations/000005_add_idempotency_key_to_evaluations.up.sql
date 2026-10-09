-- Migration: 000005_add_idempotency_key_to_evaluations.up.sql
-- Add idempotency_key column and change deduplication key from correlation_id to idempotency_key
-- Per API standard §16 INV-08: Idempotency-Key is the replay protection token

ALTER TABLE evidence_evaluations
    ADD COLUMN idempotency_key VARCHAR(255) NOT NULL DEFAULT '';

-- Backfill existing rows with their correlation_id (best effort for existing data)
UPDATE evidence_evaluations
SET idempotency_key = correlation_id
WHERE idempotency_key = '';

-- Make it non-nullable after backfill
ALTER TABLE evidence_evaluations
    ALTER COLUMN idempotency_key SET NOT NULL;

-- Drop old unique index on (tenant_id, correlation_id)
DROP INDEX IF EXISTS idx_evidence_evaluations_tenant_correlation;

-- Add new unique index on (tenant_id, idempotency_key)
CREATE UNIQUE INDEX idx_evidence_evaluations_tenant_idempotency
    ON evidence_evaluations (tenant_id, idempotency_key);

-- Keep correlation_id column for traceability but remove uniqueness