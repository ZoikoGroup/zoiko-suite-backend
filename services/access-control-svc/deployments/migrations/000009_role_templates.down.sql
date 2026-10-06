-- Migration 000009 down.
DROP INDEX IF EXISTS idx_permission_bundle_defs_one_template_bundle;
ALTER TABLE permission_bundle_defs DROP COLUMN IF EXISTS template_version;
ALTER TABLE permission_bundle_defs DROP COLUMN IF EXISTS template_code;
ALTER TABLE role_definitions       DROP COLUMN IF EXISTS template_version;
ALTER TABLE role_definitions       DROP COLUMN IF EXISTS template_code;
DROP TABLE IF EXISTS role_template_versions;
DROP FUNCTION IF EXISTS role_template_versions_immutable();
DROP TABLE IF EXISTS role_templates;
