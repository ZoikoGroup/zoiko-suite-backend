-- 000007_webhook_dlq_and_housekeeping.down.sql
-- Revert Webhook DLQ and Housekeeping Policies

DROP INDEX IF EXISTS idx_delivery_attempts_provider_msg;
DROP INDEX IF EXISTS idx_message_intents_stale_cleanup;
DROP INDEX IF EXISTS idx_action_tokens_terminal_purge;
DROP INDEX IF EXISTS idx_action_tokens_expired_active;

DROP POLICY IF EXISTS platform_scope_read_policy ON delivery_attempts;
DROP POLICY IF EXISTS platform_scope_read_policy ON message_intents;
DROP POLICY IF EXISTS platform_scope_read_policy ON action_tokens;

DROP TABLE IF EXISTS webhook_dlq CASCADE;
