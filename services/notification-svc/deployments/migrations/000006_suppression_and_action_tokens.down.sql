-- 000006_suppression_and_action_tokens.down.sql
-- Revert Suppression and Action Tokens tables

DROP TABLE IF EXISTS action_tokens CASCADE;
DROP TABLE IF EXISTS email_suppressions CASCADE;
