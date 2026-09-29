-- 000011_record_versions.down.sql

ALTER TABLE entity_jurisdiction_assignments DROP COLUMN IF EXISTS record_version;
ALTER TABLE entity_hierarchies DROP COLUMN IF EXISTS record_version;
ALTER TABLE workspaces DROP COLUMN IF EXISTS record_version;
