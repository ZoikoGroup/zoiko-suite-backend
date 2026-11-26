-- Reverts 000029. Refused while any release is recorded (a NULL new owner):
-- those history rows cannot be given an owner they never had.

COMMENT ON COLUMN work_item_assignments.new_assignee_id IS NULL;
ALTER TABLE work_item_assignments ALTER COLUMN new_assignee_id SET NOT NULL;
