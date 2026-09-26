-- Reverse of 000006.
--
-- Dropping the PENDING_UNKNOWN branch abandons every notification
-- currently ambiguous: those rows have no valid status to fall back to.
-- Resolve every PENDING_UNKNOWN row (or fail them explicitly) before
-- running this.

DROP INDEX IF EXISTS idx_notifications_pending_unknown;

ALTER TABLE notifications DROP COLUMN IF EXISTS rendered_content_hash;
ALTER TABLE notifications DROP COLUMN IF EXISTS template_version_id;
ALTER TABLE notifications DROP COLUMN IF EXISTS template_id;

ALTER TABLE notifications DROP COLUMN IF EXISTS resolution_note;
ALTER TABLE notifications DROP COLUMN IF EXISTS resolved_by_principal_id;
ALTER TABLE notifications DROP COLUMN IF EXISTS resolved_at;
ALTER TABLE notifications DROP COLUMN IF EXISTS unknown_at;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_pending_unknown_reason;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_known;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_status_known
    CHECK (status IN ('PENDING', 'SENT', 'FAILED')) NOT VALID;
