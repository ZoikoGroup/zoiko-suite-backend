-- 000006_idempotency_principal_scope.up.sql
-- Idempotency is scoped to (tenant, principal, key): a key never replays a
-- response produced for another principal. Existing rows predate principal
-- scoping and keep principal_id = '' — they can never match a real principal,
-- so they are inert (never replayed across principals).

ALTER TABLE rights_idempotency_keys
    ADD COLUMN IF NOT EXISTS principal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE rights_idempotency_keys DROP CONSTRAINT IF EXISTS rights_idempotency_keys_pkey;
ALTER TABLE rights_idempotency_keys
    ADD CONSTRAINT rights_idempotency_keys_pkey PRIMARY KEY (tenant_id, principal_id, idempotency_key);
