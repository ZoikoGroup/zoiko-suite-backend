-- Migration 000011: the runtime role can append to the outbox, and can count
-- its backlog, without being able to read or rewrite events.
--
-- 000008 put a single policy on outbox: FOR ALL USING (false). For a role
-- that is subject to row-level security that blocks INSERT as well as SELECT
-- (an INSERT is checked against WITH CHECK, which defaults to USING). The
-- service's runtime role, app_evidence_requirements (create-app-roles.sh), is
-- NOSUPERUSER, NOBYPASSRLS and not the table owner, so every SATISFIED or
-- MISSING evaluation -- which writes its event into the outbox in the same
-- transaction -- failed with "new row violates row-level security policy for
-- table outbox" and was answered 503 (verified on the local stack, 5 Nov
-- 2026). The policy only ever let rows in while the service connected as the
-- postgres superuser.
--
-- The worker reads and marks rows only through the SECURITY DEFINER functions
-- of 000008, so the runtime role needs INSERT and nothing else. The outbox is
-- a platform table (one row per event, no tenant column), hence WITH CHECK
-- (true) rather than a tenant predicate; the event's tenant is in its payload
-- and in the Kafka key.
DROP POLICY IF EXISTS tenant_isolation_policy ON outbox;
CREATE POLICY outbox_append_only ON outbox
    FOR INSERT
    WITH CHECK (true);

-- The console's KPI row and the evidence_outbox_unpublished gauge need the
-- backlog size. The runtime role cannot SELECT the table (no SELECT policy),
-- so the count is a SECURITY DEFINER function, like claim_outbox_events.
CREATE OR REPLACE FUNCTION outbox_unpublished_count(older_than INTERVAL DEFAULT INTERVAL '0 seconds')
RETURNS BIGINT AS $$
    SELECT count(*) FROM outbox
     WHERE published_at IS NULL
       AND created_at <= now() - older_than;
$$ LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp;

-- Functions are executable by PUBLIC by default; state the runtime grants
-- explicitly, and only where the role exists (CI databases have neither).
DO $$
DECLARE r TEXT;
BEGIN
    FOREACH r IN ARRAY ARRAY['app_evidence_requirements', 'zoiko_app'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT INSERT ON outbox TO %I', r);
            EXECUTE format('GRANT EXECUTE ON FUNCTION outbox_unpublished_count(INTERVAL) TO %I', r);
        END IF;
    END LOOP;
END
$$;
