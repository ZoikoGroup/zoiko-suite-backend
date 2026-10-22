-- 000014_add_document_idempotency_keys.down.sql

DROP TABLE IF EXISTS document_idempotency_keys;
DROP FUNCTION IF EXISTS reject_idempotency_key_mutation();
