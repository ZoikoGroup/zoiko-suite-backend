-- +migrate Up
BEGIN;

-- BIZ-08 Wave 2. DeadlineDueSoon/DeadlineOverdue are named events, but
-- (per this service's own established doctrine on SLAState — see
-- migration 000003's doc comment) there is no background clock service
-- in this codebase, and inventing one here would be fabricating scope no
-- command names. Upcoming/Due/Overdue are computed live, not stored —
-- so instead of a scheduler, ListUpcoming/ListOverdue THEMSELVES are the
-- one legitimate observation point: the first read that notices a
-- deadline has crossed a threshold records it here and the caller
-- publishes exactly once. Any later read of the same still-crossed
-- deadline sees the timestamp already set and does not republish.
ALTER TABLE deadlines ADD COLUMN due_soon_notified_at TIMESTAMPTZ;
ALTER TABLE deadlines ADD COLUMN overdue_notified_at TIMESTAMPTZ;

COMMIT;
