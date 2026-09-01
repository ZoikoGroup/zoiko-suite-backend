-- Reverse of 000004.
--
-- Dropping next_attempt_at abandons every notification currently awaiting a
-- retry: those rows stay PENDING forever, with nothing scheduled to move them
-- and no record that anything was going to. Conclude or fail them before
-- running this, or they become a silent backlog of notices that were never
-- sent and never reported as unsent.

-- Both names: this policy was briefly created as platform_scope_read_policy on
-- Supabase before being renamed to platform_scope_read there, and a database
-- migrated in between could carry either.
DROP POLICY IF EXISTS platform_scope_read_policy ON notifications;
DROP POLICY IF EXISTS platform_scope_read ON notifications;

DROP INDEX IF EXISTS idx_notifications_due_retry;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_attempts_non_negative;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_concluded_has_no_retry;

ALTER TABLE notifications DROP COLUMN IF EXISTS last_attempt_at;
ALTER TABLE notifications DROP COLUMN IF EXISTS next_attempt_at;
ALTER TABLE notifications DROP COLUMN IF EXISTS delivery_attempts;
