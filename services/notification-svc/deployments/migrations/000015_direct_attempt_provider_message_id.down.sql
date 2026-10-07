-- 000015 down: removes the direct-attempt provider message id and its index.
DROP INDEX IF EXISTS uq_nda_provider_message_id;
ALTER TABLE notification_delivery_attempts DROP COLUMN IF EXISTS provider_message_id;
