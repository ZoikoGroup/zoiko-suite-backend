-- 000004_org_unit_closed_at.down.sql
ALTER TABLE org_unit_manager_refs       DROP COLUMN IF EXISTS closed_at;
ALTER TABLE org_unit_hierarchy_versions DROP COLUMN IF EXISTS closed_at;
