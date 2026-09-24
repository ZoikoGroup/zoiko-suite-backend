-- Revert the two access_decision_log maintenance helpers to SECURITY INVOKER.
--
-- After this runs the service role can no longer create or detach partitions —
-- which is 000009's shipped behaviour, and the defect 000011 exists to fix. Any
-- scheduled retention sweep will start reporting "permission denied for schema
-- public" on the create half and "must be owner of table" on the detach half.
-- That is the intended meaning of reverting this migration, not a surprise.
--
-- The EXECUTE grants are dropped and EXECUTE is restored to PUBLIC, because
-- that is what CREATE FUNCTION had left in place before 000011 narrowed it.
-- Restoring it is only sound BECAUSE this reverts SECURITY DEFINER in the same
-- transaction: EXECUTE to PUBLIC on a SECURITY INVOKER function grants nothing
-- the caller does not already hold.

BEGIN;

ALTER FUNCTION create_access_decision_log_partition(DATE)
    SECURITY INVOKER
    RESET search_path;

ALTER FUNCTION detach_access_decision_log_partitions_before(DATE)
    SECURITY INVOKER
    RESET search_path;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        REVOKE EXECUTE ON FUNCTION create_access_decision_log_partition(DATE)
            FROM app_authorization;
        REVOKE EXECUTE ON FUNCTION detach_access_decision_log_partitions_before(DATE)
            FROM app_authorization;

        IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'access_decision_log_retention_status') THEN
            REVOKE SELECT ON access_decision_log_retention_status FROM app_authorization;
        END IF;
    END IF;
END
$$;

GRANT EXECUTE ON FUNCTION create_access_decision_log_partition(DATE) TO PUBLIC;
GRANT EXECUTE ON FUNCTION detach_access_decision_log_partitions_before(DATE) TO PUBLIC;

COMMENT ON FUNCTION create_access_decision_log_partition(DATE) IS
    'Creates the monthly access_decision_log partition covering month_start, with row security enabled and the parent''s tenant policy applied. Idempotent.';

COMMENT ON FUNCTION detach_access_decision_log_partitions_before(DATE) IS
    'Detaches every access_decision_log monthly partition whose range ends on or before cutoff, returning (partition_name, row_count). Detached partitions remain as ordinary tables: archive, then drop them deliberately. Never detaches access_decision_log_default.';

COMMIT;
