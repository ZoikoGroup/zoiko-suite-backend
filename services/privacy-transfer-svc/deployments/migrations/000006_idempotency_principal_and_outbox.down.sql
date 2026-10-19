-- 000006_idempotency_principal_and_outbox.down.sql
DROP TABLE IF EXISTS event_outbox;
ALTER TABLE transfer_idempotency_keys DROP CONSTRAINT IF EXISTS transfer_idempotency_keys_pkey;
ALTER TABLE transfer_idempotency_keys ADD CONSTRAINT transfer_idempotency_keys_pkey PRIMARY KEY (tenant_id, idempotency_key);
ALTER TABLE transfer_idempotency_keys DROP COLUMN IF EXISTS principal_id;
