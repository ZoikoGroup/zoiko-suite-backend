-- 000031: published outbox rows and old idempotency keys are purged
-- (Governance audit N-14, 13 Oct 2026).
--
-- Both tables only ever grew. outbox_events gains a row per authorization
-- decision and per IAM change, and keeps it after the relay has published it;
-- idempotency_keys keeps every admin command's key forever. The retention
-- sweeper now calls the two functions below.
--
-- WHY FUNCTIONS. The service role holds no DELETE on outbox_events, and
-- idempotency_keys is FORCE ROW LEVEL SECURITY with a per-tenant policy, so no
-- ordinary statement could purge across tenants. The functions are SECURITY
-- DEFINER, and idempotency_keys gets a policy that admits ONLY a statement
-- running inside such a function with the purge flag set:
--   current_user <> session_user  — true inside a SECURITY DEFINER function
--                                   owned by another role, false for the
--                                   service role's own statements;
--   app.idempotency_purge = 'on'  — set by the function, transaction-local.
-- A plain session setting the flag itself still has current_user =
-- session_user and is refused, so the policy is not a cross-tenant hole.
-- FOR ALL, not FOR DELETE: a DELETE with a WHERE clause must also pass the
-- SELECT policies, and the tenant policy hides every other tenant's rows, so a
-- DELETE-only policy purges nothing unless the function's owner is a superuser
-- (measured on a NOSUPERUSER owner).
--
-- Pending outbox rows are never purged: only rows with published_at set, and
-- only after the retention interval. A floor of one hour is enforced in SQL.

BEGIN;

CREATE OR REPLACE FUNCTION authz_purge_published_outbox(older_than INTERVAL)
RETURNS BIGINT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    n BIGINT;
BEGIN
    IF older_than IS NULL OR older_than < INTERVAL '1 hour' THEN
        RAISE EXCEPTION 'authz_purge_published_outbox: retention below one hour refused (%)', older_than;
    END IF;
    DELETE FROM outbox_events
     WHERE published_at IS NOT NULL
       AND published_at < NOW() - older_than;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;

CREATE OR REPLACE FUNCTION authz_purge_idempotency_keys(older_than INTERVAL)
RETURNS BIGINT
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    n BIGINT;
BEGIN
    IF older_than IS NULL OR older_than < INTERVAL '1 hour' THEN
        RAISE EXCEPTION 'authz_purge_idempotency_keys: retention below one hour refused (%)', older_than;
    END IF;
    PERFORM set_config('app.idempotency_purge', 'on', true);
    DELETE FROM idempotency_keys WHERE created_at < NOW() - older_than;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM set_config('app.idempotency_purge', '', true);
    RETURN n;
END;
$$;

DROP POLICY IF EXISTS idempotency_keys_purge ON idempotency_keys;
CREATE POLICY idempotency_keys_purge ON idempotency_keys FOR ALL
    USING (current_setting('app.idempotency_purge', true) = 'on'
           AND current_user <> session_user);

REVOKE EXECUTE ON FUNCTION authz_purge_published_outbox(INTERVAL) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION authz_purge_idempotency_keys(INTERVAL) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT EXECUTE ON FUNCTION authz_purge_published_outbox(INTERVAL) TO app_authorization;
        GRANT EXECUTE ON FUNCTION authz_purge_idempotency_keys(INTERVAL) TO app_authorization;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT EXECUTE ON FUNCTION authz_purge_published_outbox(INTERVAL) TO zoiko_app;
        GRANT EXECUTE ON FUNCTION authz_purge_idempotency_keys(INTERVAL) TO zoiko_app;
    END IF;
END
$$;

COMMIT;
