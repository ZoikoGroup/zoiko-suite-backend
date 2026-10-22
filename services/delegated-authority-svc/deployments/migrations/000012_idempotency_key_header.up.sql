-- Idempotency-Key HTTP header as part of the dedup key.
-- A retry with a fresh correlation_id but a repeated Idempotency-Key header
-- previously created a second grant because idempotency was scoped only by
-- (tenant_id, correlation_id, created_by_principal_id, action_type,
-- legal_entity_id, effective_from, effective_to). The Idempotency-Key header
-- is now part of the composite key so a repeated header value means a replay,
-- not a new grant.

ALTER TABLE delegation_grants ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(255);

DROP INDEX IF EXISTS idx_delegation_grants_idempotency;

CREATE UNIQUE INDEX idx_delegation_grants_idempotency
    ON delegation_grants (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id,
                          effective_from, effective_to, idempotency_key);
