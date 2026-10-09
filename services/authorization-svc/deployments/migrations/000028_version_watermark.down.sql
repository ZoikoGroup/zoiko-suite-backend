DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['authz_config_history', 'sod_exceptions', 'principal_role_assignments',
                             'delegated_authorities', 'principal_status_projection', 'entity_status_projection'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t || '_bump_watermark', t);
    END LOOP;
END
$$;
DROP FUNCTION IF EXISTS authz_bump_version();
DROP TABLE IF EXISTS authz_versions;
