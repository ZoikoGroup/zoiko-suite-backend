-- 000013_add_idempotency_key.up.sql
-- Add idempotency_key column for proper idempotency support
-- per API standard §16 INV-08 and GCP §16

ALTER TABLE workflow_instances
    ADD COLUMN idempotency_key TEXT;

-- Unique constraint for idempotency: one workflow per (tenant_id, idempotency_key)
-- Only enforced when idempotency_key is provided
CREATE UNIQUE INDEX idx_workflow_instances_idempotency_key 
    ON workflow_instances (tenant_id, idempotency_key) 
    WHERE idempotency_key IS NOT NULL AND idempotency_key != '';