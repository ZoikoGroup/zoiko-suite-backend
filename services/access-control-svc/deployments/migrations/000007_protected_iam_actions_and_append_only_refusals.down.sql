-- Migration 000007 down.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT UPDATE, DELETE ON refused_escalations TO zoiko_app;
        GRANT INSERT, UPDATE, DELETE ON protected_permissions TO zoiko_app;
    END IF;
END
$$;

DROP POLICY IF EXISTS refused_escalations_append ON refused_escalations;
DROP POLICY IF EXISTS refused_escalations_read ON refused_escalations;
CREATE POLICY tenant_isolation_policy ON refused_escalations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE refused_escalations   NO FORCE ROW LEVEL SECURITY;
ALTER TABLE protected_permissions NO FORCE ROW LEVEL SECURITY;

DELETE FROM protected_permissions WHERE action_name LIKE 'iam.%';
