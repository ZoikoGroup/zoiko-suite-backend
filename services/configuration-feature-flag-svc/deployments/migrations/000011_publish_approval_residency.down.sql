ALTER TABLE config_definitions DROP COLUMN IF EXISTS allowed_regions;
ALTER TABLE config_definition_versions
    DROP COLUMN IF EXISTS approved_by_principal_id,
    DROP COLUMN IF EXISTS approval_reference;
