-- Down migration for 000008_idempotency_scope.up.sql

-- Drop the new index
DROP INDEX IF EXISTS idx_delegation_grants_idempotency;

-- Recreate the old index
CREATE UNIQUE INDEX idx_delegation_grants_tenant_correlation ON delegation_grants (tenant_id, correlation_id);