-- 000012_add_row_version.down.sql
-- Rollback row_version and common columns

ALTER TABLE workflow_instances
    DROP COLUMN IF EXISTS row_version,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS plane,
    DROP COLUMN IF EXISTS data_class,
    DROP COLUMN IF EXISTS residency_region;

ALTER TABLE workflow_stages
    DROP COLUMN IF EXISTS row_version,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS plane,
    DROP COLUMN IF EXISTS data_class,
    DROP COLUMN IF EXISTS residency_region;

ALTER TABLE workflow_transitions
    DROP COLUMN IF EXISTS row_version,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS plane,
    DROP COLUMN IF EXISTS data_class,
    DROP COLUMN IF EXISTS residency_region;