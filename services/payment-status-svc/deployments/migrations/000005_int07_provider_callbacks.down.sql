DROP TABLE IF EXISTS provider_callback_receipts;
ALTER TABLE payment_execution_states DROP COLUMN IF EXISTS provider_key, DROP COLUMN IF EXISTS last_provider_event_at;
