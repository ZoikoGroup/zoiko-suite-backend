-- Reverse of 000013. Without the marker the stranded sweep can no longer tell a
-- lost submission from one that never started, and goes back to re-sending
-- both. Roll the binary back with it: the store writes this column.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_submitting_only_pending;
ALTER TABLE notifications DROP COLUMN IF EXISTS submitting_since;
