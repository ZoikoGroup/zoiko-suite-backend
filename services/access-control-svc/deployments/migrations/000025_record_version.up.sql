-- 000025_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to all mutable aggregates
-- in access-control-svc. API-3 requires ETag/If-Match on every aggregate mutation;
-- GET must return ETag, and mutations require If-Match.

ALTER TABLE role_definitions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE permission_bundle_defs
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE protected_permissions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE assignment_requests
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE groups
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE group_assignments
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE group_members
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE review_campaigns
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE review_items
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE role_definitions
    ADD CONSTRAINT role_def_version_positive CHECK (record_version > 0);

ALTER TABLE permission_bundle_defs
    ADD CONSTRAINT perm_bundle_def_version_positive CHECK (record_version > 0);

ALTER TABLE protected_permissions
    ADD CONSTRAINT prot_perm_version_positive CHECK (record_version > 0);

ALTER TABLE assignment_requests
    ADD CONSTRAINT assign_req_version_positive CHECK (record_version > 0);

ALTER TABLE groups
    ADD CONSTRAINT group_version_positive CHECK (record_version > 0);

ALTER TABLE group_assignments
    ADD CONSTRAINT grp_assign_version_positive CHECK (record_version > 0);

ALTER TABLE group_members
    ADD CONSTRAINT grp_member_version_positive CHECK (record_version > 0);

ALTER TABLE review_campaigns
    ADD CONSTRAINT rev_camp_version_positive CHECK (record_version > 0);

ALTER TABLE review_items
    ADD CONSTRAINT rev_item_version_positive CHECK (record_version > 0);