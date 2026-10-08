-- 000026 down: removes the history sequence.
DROP INDEX IF EXISTS idx_notice_events_seq;
ALTER TABLE regulated_notice_events DROP COLUMN IF EXISTS seq;
