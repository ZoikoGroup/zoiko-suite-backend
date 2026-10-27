-- 000032: re-assert EXECUTE on the SECURITY DEFINER maintenance functions
-- (Security & Trust / authorization-svc pass of 13 Oct 2026, finding AZ-P2).
--
-- 000011 and 000031 REVOKE EXECUTE on their definer functions FROM PUBLIC and
-- then GRANT it to the service role BY NAME, guarded on the role existing.
-- On a fresh volume init-db.sh applies every migration BEFORE
-- create-app-roles.sh creates app_authorization, so the guard skips the GRANT
-- and only the REVOKE lands. Measured on the dev stack: the service logged
--   permission denied for function create_access_decision_log_partition
-- on every retention pass, so no monthly decision-log partition was created
-- and no old partition detached (decisions fell into the default partition).
-- 000031's purge functions are lost the same way on any fresh volume.
--
-- This migration repeats the grants. It is idempotent, and the tracked
-- migrations runner (deployments/migrate.py) runs it once the roles exist,
-- which repairs every existing volume. A fresh volume still needs the roles
-- created before migrations, or this file run again afterwards — recorded in
-- the audit as a platform (init-db.sh) item, not fixable from a migration.

DO $$
DECLARE
    fn TEXT;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        FOREACH fn IN ARRAY ARRAY[
            'create_access_decision_log_partition(DATE)',
            'detach_access_decision_log_partitions_before(DATE)',
            'authz_purge_published_outbox(INTERVAL)',
            'authz_purge_idempotency_keys(INTERVAL)'
        ] LOOP
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO app_authorization', fn);
        END LOOP;
        IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'access_decision_log_retention_status') THEN
            GRANT SELECT ON access_decision_log_retention_status TO app_authorization;
        END IF;
    ELSE
        RAISE NOTICE 'role app_authorization not present: re-run 000032 after create-app-roles.sh';
    END IF;
    -- 000031 also granted the purge functions to the shared runtime role.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT EXECUTE ON FUNCTION authz_purge_published_outbox(INTERVAL) TO zoiko_app;
        GRANT EXECUTE ON FUNCTION authz_purge_idempotency_keys(INTERVAL) TO zoiko_app;
    END IF;
END
$$;
