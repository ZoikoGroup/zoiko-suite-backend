-- 000007_idempotency_principal_scope.down.sql
ALTER TABLE consent_idempotency_keys DROP CONSTRAINT IF EXISTS consent_idempotency_keys_pkey;
ALTER TABLE consent_idempotency_keys ADD CONSTRAINT consent_idempotency_keys_pkey PRIMARY KEY (tenant_id, idempotency_key);
ALTER TABLE consent_idempotency_keys DROP COLUMN IF EXISTS principal_id;
