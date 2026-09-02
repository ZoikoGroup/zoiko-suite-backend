-- Reverse of 000003.
--
-- This down migration destroys evidence: recipient_address and
-- provider_response are the record of where a notice went and what accepted
-- it, and read_at is the record of the recipient acknowledging it. Dropping
-- the columns discards those facts for every notification ever sent, and no
-- re-application of the up migration recovers them.
--
-- It exists because the migration tool requires a pair and a missing down file
-- fails the run. Reaching for it on an environment that has delivered anything
-- real is a decision with a person attached, not a rollback step.

DROP INDEX IF EXISTS idx_notifications_unread;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_read_after_created;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_read_state_is_in_app;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_address_has_provenance;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_address_source_known;

ALTER TABLE notifications DROP COLUMN IF EXISTS read_at;
ALTER TABLE notifications DROP COLUMN IF EXISTS provider_response;
ALTER TABLE notifications DROP COLUMN IF EXISTS recipient_address_source;
ALTER TABLE notifications DROP COLUMN IF EXISTS recipient_address;
