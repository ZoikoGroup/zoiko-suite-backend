DROP INDEX IF EXISTS idx_assignment_requests_group;
ALTER TABLE assignment_requests DROP COLUMN IF EXISTS group_assignment_id;
DROP TABLE IF EXISTS iam_group_assignments;
DROP TABLE IF EXISTS iam_group_members;
DROP TABLE IF EXISTS iam_groups;
