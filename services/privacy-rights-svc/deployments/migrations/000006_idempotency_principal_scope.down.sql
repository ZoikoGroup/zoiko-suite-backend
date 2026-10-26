-- 000006_idempotency_principal_scope.down.sql
ALTER TABLE rights_idempotency_keys DROP CONSTRAINT IF EXISTS rights_idempotency_keys_pkey;
ALTER TABLE rights_idempotency_keys ADD CONSTRAINT rights_idempotency_keys_pkey PRIMARY KEY (tenant_id, idempotency_key);
ALTER TABLE rights_idempotency_keys DROP COLUMN IF EXISTS principal_id;
