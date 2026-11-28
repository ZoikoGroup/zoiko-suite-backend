-- Migration 000029: a work item release is recorded in its assignment history.
--
-- work_item_assignments (000024) keeps every change of owner: previous owner,
-- new owner, actor, time. Releasing a claimed item hands it back to the queue,
-- so it has no new owner, but new_assignee_id was NOT NULL: every
-- POST /v1/work-items/{id}/release failed with 503 (found by the live test of
-- 26 Nov 2026). NULL now means "returned to the queue"; a claim or a
-- reassignment still names the new owner.

ALTER TABLE work_item_assignments ALTER COLUMN new_assignee_id DROP NOT NULL;

COMMENT ON COLUMN work_item_assignments.new_assignee_id IS
    'The owner after this change; NULL when the item was released back to its queue.';
