-- 000006_idempotency_principal_scope.down.sql
-- Restoring the (tenant, key) key can fail if two principals used the same key.
ALTER TABLE decision_idempotency_keys
    DROP CONSTRAINT IF EXISTS decision_idempotency_keys_pkey;
ALTER TABLE decision_idempotency_keys
    ADD CONSTRAINT decision_idempotency_keys_pkey PRIMARY KEY (tenant_id, idempotency_key);
ALTER TABLE decision_idempotency_keys DROP COLUMN IF EXISTS principal_id;
