-- Make the access_decision_log retention helpers callable by the service.
--
-- ── THE DEFECT ──────────────────────────────────────────────────────────────
--
-- 000009 built the partitioning and both maintenance helpers, and its own notes
-- describe them as what "the retention job needs every month". Neither could be
-- called by the service that needs them.
--
-- Both are LANGUAGE plpgsql with no SECURITY clause, so they execute as the
-- CALLER. access_decision_log is owned by the migration role, and the service
-- connects as app_authorization — which create-app-roles.sh deliberately gives
-- USAGE on the schema and DML on tables and NO DDL, correctly. So the bodies
-- failed on the DDL inside them:
--
--   create_access_decision_log_partition('2027-03-01')
--     ERROR:  permission denied for schema public
--     QUERY:  CREATE TABLE access_decision_log_2027_03 PARTITION OF ...
--
--   detach_access_decision_log_partitions_before('2026-10-01')
--     ERROR:  must be owner of table access_decision_log
--     CONTEXT: ALTER TABLE access_decision_log DETACH PARTITION ...
--
-- Both measured against the running compose stack, connected as
-- app_authorization. EXECUTE on the functions was never the problem — the
-- failures are from inside the bodies.
--
-- 000009's tests passed throughout because they run on the migration
-- connection, which owns the table. This is the same blind spot migration
-- 000008 was written to close for row security: a suite that only uses the
-- migration role proves nothing about the role the service actually uses.
--
-- Consequence: the partition runway could only ever be extended by hand. When
-- the last pre-created month elapsed, every decision would land in
-- access_decision_log_default — caught rather than an outage, by 000009's
-- design, but silently and with the runway gone.
--
-- ── THE FIX ─────────────────────────────────────────────────────────────────
--
-- SECURITY DEFINER on both, so the DDL inside executes as the function owner
-- (the migration role, which owns the parent table) rather than as the caller.
-- This is the standard shape for a maintenance operation a deliberately
-- DDL-less service role must be able to trigger: the privilege stays with the
-- function, not with the role.
--
-- Bodies are NOT changed. They are already safe to run this way — every
-- identifier goes through format(%I), both parameters are DATE-typed so no
-- string reaches SQL unescaped, and neither function takes a table name.

BEGIN;

-- search_path is pinned on both. A SECURITY DEFINER function inherits the
-- caller's search_path unless told otherwise, and a caller able to prepend a
-- schema could shadow a function or operator the body resolves and have it run
-- with the definer's privileges. pg_temp goes last, explicitly: it is
-- searched ahead of everything by default, and a temporary object is exactly
-- what an attacker can create.
ALTER FUNCTION create_access_decision_log_partition(DATE)
    SECURITY DEFINER
    SET search_path = public, pg_temp;

ALTER FUNCTION detach_access_decision_log_partitions_before(DATE)
    SECURITY DEFINER
    SET search_path = public, pg_temp;

-- Postgres grants EXECUTE on a new function to PUBLIC. That is bounded here by
-- create-app-roles.sh having REVOKEd CONNECT on this database from PUBLIC, so
-- only a role granted CONNECT can reach these at all — but "bounded by another
-- object's grants" is not the same as scoped, and these two now run with the
-- table owner's privileges. Narrowed to the role that needs them, following
-- the same REVOKE-from-PUBLIC-first doctrine create-app-roles.sh applies to
-- CONNECT.
REVOKE EXECUTE ON FUNCTION create_access_decision_log_partition(DATE) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION detach_access_decision_log_partitions_before(DATE) FROM PUBLIC;

-- Granted by name, if that name exists. The role is configurable
-- (DB_USER_AUTHORIZATION, default app_authorization) and a migration cannot
-- know a deployment's choice, so this is conditional rather than an error: a
-- deployment using a different role name grants EXECUTE to it explicitly, and
-- one still connecting as a superuser is unaffected either way.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT EXECUTE ON FUNCTION create_access_decision_log_partition(DATE)
            TO app_authorization;
        GRANT EXECUTE ON FUNCTION detach_access_decision_log_partitions_before(DATE)
            TO app_authorization;
    ELSE
        RAISE NOTICE 'role app_authorization not present; grant EXECUTE on the '
                     'two access_decision_log maintenance functions to this '
                     'deployment''s service role manually';
    END IF;
END
$$;

-- The status view is a plain read over pg_catalog and the parent table, so it
-- needs no definer rights — but the service role does need to select from it,
-- and a view created by a later migration is not covered by the ALTER DEFAULT
-- PRIVILEGES in create-app-roles.sh (that names TABLES, which does include
-- views, but only for objects created by the migration role AFTER the ALTER
-- ran — and 000009 created this one). Granted explicitly so the retention job
-- can report the runway it is maintaining.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization')
       AND EXISTS (SELECT 1 FROM pg_class WHERE relname = 'access_decision_log_retention_status') THEN
        GRANT SELECT ON access_decision_log_retention_status TO app_authorization;
    END IF;
END
$$;

COMMENT ON FUNCTION create_access_decision_log_partition(DATE) IS
    'Creates the monthly access_decision_log partition covering month_start, with row security enabled and the parent''s tenant policy applied. Idempotent. SECURITY DEFINER since 000011: the service role holds no DDL, so the privilege lives with the function.';

COMMENT ON FUNCTION detach_access_decision_log_partitions_before(DATE) IS
    'Detaches every access_decision_log monthly partition whose range ends on or before cutoff, returning (partition_name, row_count). Detached partitions remain as ordinary tables: archive, then drop them deliberately. Never detaches access_decision_log_default. SECURITY DEFINER since 000011: DETACH requires ownership of the parent, which the service role does not and should not have.';

COMMIT;
