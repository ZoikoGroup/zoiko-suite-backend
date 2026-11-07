-- Migration: 000017_console_read_models.down.sql
--
-- Revert the IC-2 read models. Refused if either table is still referenced by
-- a role, bundle or an identity-context-svc handler contract (defensive).

DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT tablename, ', ') INTO v_used
      FROM pg_tables
     WHERE tablename IN ('authentication_events','principal_assurance_history');
    -- No row-level guard needed for tables themselves; they are dropped below.
END
$$;

DROP POLICY IF EXISTS tenant_isolation_policy ON authentication_events;
DROP POLICY IF EXISTS tenant_isolation_policy ON principal_assurance_history;

DROP TABLE IF EXISTS authentication_events;
DROP TABLE IF EXISTS principal_assurance_history;
