-- 000025_record_version.down.sql
--
-- Revert record_version additions for all mutable aggregates.

ALTER TABLE review_items
    DROP CONSTRAINT IF EXISTS rev_item_version_positive;
ALTER TABLE review_items
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE review_campaigns
    DROP CONSTRAINT IF EXISTS rev_camp_version_positive;
ALTER TABLE review_campaigns
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE group_members
    DROP CONSTRAINT IF EXISTS grp_member_version_positive;
ALTER TABLE group_members
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE group_assignments
    DROP CONSTRAINT IF EXISTS grp_assign_version_positive;
ALTER TABLE group_assignments
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS group_version_positive;
ALTER TABLE groups
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE assignment_requests
    DROP CONSTRAINT IF EXISTS assign_req_version_positive;
ALTER TABLE assignment_requests
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE protected_permissions
    DROP CONSTRAINT IF EXISTS prot_perm_version_positive;
ALTER TABLE protected_permissions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE permission_bundle_defs
    DROP CONSTRAINT IF EXISTS perm_bundle_def_version_positive;
ALTER TABLE permission_bundle_defs
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE role_definitions
    DROP CONSTRAINT IF EXISTS role_def_version_positive;
ALTER TABLE role_definitions
    DROP COLUMN IF EXISTS record_version;