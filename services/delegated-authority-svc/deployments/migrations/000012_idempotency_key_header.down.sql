DROP INDEX IF EXISTS idx_delegation_grants_idempotency;

CREATE UNIQUE INDEX idx_delegation_grants_idempotency
    ON delegation_grants (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id,
                          effective_from, effective_to);

ALTER TABLE delegation_grants DROP COLUMN IF EXISTS idempotency_key;
