ALTER TABLE project_recognition_runs
    DROP COLUMN IF EXISTS wip_mapping_key,
    DROP COLUMN IF EXISTS revenue_mapping_key;
