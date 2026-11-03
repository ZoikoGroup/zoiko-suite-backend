-- 000025_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to all mutable aggregates
-- in access-control-svc. API-3 requires ETag/If-Match on every aggregate mutation;
-- GET must return ETag, and mutations require If-Match.
--
-- The group and review tables are iam_groups / iam_group_assignments /
-- iam_group_members (000014) and access_review_campaigns /
-- access_review_items (000011). The first version of this file named them
-- groups / group_assignments / group_members / review_campaigns / review_items,
-- which do not exist, so it failed on every database after adding the column
-- to the first four tables (it ran without a transaction). IF NOT EXISTS and
-- the DROP-then-ADD constraints let it run cleanly over that partial state.

BEGIN;

ALTER TABLE role_definitions
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE permission_bundle_defs
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE protected_permissions
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE assignment_requests
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE iam_groups
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE iam_group_assignments
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE iam_group_members
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE access_review_campaigns
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE access_review_items
    ADD COLUMN IF NOT EXISTS record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE role_definitions
    DROP CONSTRAINT IF EXISTS role_def_version_positive,
    ADD CONSTRAINT role_def_version_positive CHECK (record_version > 0);

ALTER TABLE permission_bundle_defs
    DROP CONSTRAINT IF EXISTS perm_bundle_def_version_positive,
    ADD CONSTRAINT perm_bundle_def_version_positive CHECK (record_version > 0);

ALTER TABLE protected_permissions
    DROP CONSTRAINT IF EXISTS prot_perm_version_positive,
    ADD CONSTRAINT prot_perm_version_positive CHECK (record_version > 0);

ALTER TABLE assignment_requests
    DROP CONSTRAINT IF EXISTS assign_req_version_positive,
    ADD CONSTRAINT assign_req_version_positive CHECK (record_version > 0);

ALTER TABLE iam_groups
    DROP CONSTRAINT IF EXISTS group_version_positive,
    ADD CONSTRAINT group_version_positive CHECK (record_version > 0);

ALTER TABLE iam_group_assignments
    DROP CONSTRAINT IF EXISTS grp_assign_version_positive,
    ADD CONSTRAINT grp_assign_version_positive CHECK (record_version > 0);

ALTER TABLE iam_group_members
    DROP CONSTRAINT IF EXISTS grp_member_version_positive,
    ADD CONSTRAINT grp_member_version_positive CHECK (record_version > 0);

ALTER TABLE access_review_campaigns
    DROP CONSTRAINT IF EXISTS rev_camp_version_positive,
    ADD CONSTRAINT rev_camp_version_positive CHECK (record_version > 0);

ALTER TABLE access_review_items
    DROP CONSTRAINT IF EXISTS rev_item_version_positive,
    ADD CONSTRAINT rev_item_version_positive CHECK (record_version > 0);

COMMIT;
