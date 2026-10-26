DROP INDEX IF EXISTS idx_delegation_grants_idempotency;

CREATE UNIQUE INDEX idx_delegation_grants_idempotency
    ON delegation_grants (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id,
                          effective_from, effective_to, idempotency_key);

ALTER TABLE delegation_grants ALTER COLUMN idempotency_key DROP NOT NULL;
ALTER TABLE delegation_grants ALTER COLUMN idempotency_key DROP DEFAULT;
