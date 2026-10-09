-- Reverts 000022. Dropping authz_config_history discards the recorded
-- configuration history; take a copy first if it may be needed as evidence.
DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['roles', 'permission_bundles', 'sod_rules', 'abac_rules'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t || '_bump_version', t);
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t || '_record_history', t);
        EXECUTE format('ALTER TABLE %I DROP COLUMN IF EXISTS version', t);
    END LOOP;
END
$$;

DROP TABLE IF EXISTS authz_config_history;
DROP FUNCTION IF EXISTS authz_config_record_history();
DROP FUNCTION IF EXISTS authz_config_bump_version();
DROP FUNCTION IF EXISTS authz_config_history_immutable();
