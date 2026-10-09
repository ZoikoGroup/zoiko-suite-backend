DROP INDEX IF EXISTS idx_event_schemas_event_idempotency;
ALTER TABLE event_schemas DROP COLUMN IF EXISTS idempotency_key;
