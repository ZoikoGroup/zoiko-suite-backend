-- Idempotency scoped by principal+role+scope+validity
-- The original index was on (tenant_id, correlation_id) only.
-- Per the audit gap: "Idempotency scoped by principal+role+scope+validity" -
-- the same correlation_id should be allowed for different principals, actions,
-- entities, or time windows.

-- Drop the old index
DROP INDEX IF EXISTS idx_delegation_grants_tenant_correlation;

-- Create new index scoped by principal+role+scope+validity
-- A replay is only a replay if ALL of these match:
-- - tenant_id (already in the table)
-- - correlation_id (client idempotency key)
-- - created_by_principal_id (the caller)
-- - action_type (the role/action being delegated)
-- - legal_entity_id (the scope)
-- - effective_from (the start of the validity window)
-- - effective_to (the end of the validity window)
CREATE UNIQUE INDEX idx_delegation_grants_idempotency
    ON delegation_grants (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id, effective_from, effective_to);