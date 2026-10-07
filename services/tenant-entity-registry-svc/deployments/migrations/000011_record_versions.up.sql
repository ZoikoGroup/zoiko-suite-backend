-- 000011_record_versions.up.sql
--
-- Optimistic-concurrency versions for the three aggregates that had none:
-- workspaces, entity hierarchies and entity–jurisdiction assignments.
--
-- ORG §3 "All material master updates require optimistic concurrency" and §7
-- "event minimum payload: … object_version …". Their events (now written
-- through the transactional outbox) must carry the version they attest, and
-- there was no version to carry. Existing rows start at 1.

ALTER TABLE workspaces
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE entity_hierarchies
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE entity_jurisdiction_assignments
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE workspaces
    ADD CONSTRAINT ws_version_positive CHECK (record_version > 0);
ALTER TABLE entity_hierarchies
    ADD CONSTRAINT eh_version_positive CHECK (record_version > 0);
ALTER TABLE entity_jurisdiction_assignments
    ADD CONSTRAINT eja_version_positive CHECK (record_version > 0);
