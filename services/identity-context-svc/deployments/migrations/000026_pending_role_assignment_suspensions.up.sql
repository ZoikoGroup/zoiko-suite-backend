-- Migration 000020: holds for assignments the role projection has not seen yet.
--
-- iam.assignment.suspended and iam.assignment.resumed can arrive before
-- iam.assignment.granted. The suspension must still be recorded, or the late
-- grant frames the role back in (IC-12). A stub row in
-- principal_role_assignments cannot carry that state: principal_id is NOT NULL
-- and references principals, and an unprojected assignment has no principal
-- row, so the insert fails with a foreign-key violation.
--
-- The pending state lives here instead, keyed by tenant and assignment with no
-- principal reference. UpsertRoleAssignment merges it into the projection and
-- deletes the pending row when the grant arrives.

CREATE TABLE IF NOT EXISTS pending_role_assignment_suspensions (
    tenant_id     VARCHAR(255) NOT NULL,
    assignment_id VARCHAR(255) NOT NULL,
    suspended_at  TIMESTAMP WITH TIME ZONE,
    resumed_at    TIMESTAMP WITH TIME ZONE,
    recorded_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, assignment_id)
);

ALTER TABLE pending_role_assignment_suspensions ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_role_assignment_suspensions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON pending_role_assignment_suspensions;
CREATE POLICY tenant_isolation_policy ON pending_role_assignment_suspensions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Guarded on the role, as 000018 and 000019 are, so a database without
-- create-app-roles.sh applied (CI, the store tests) still migrates.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_identity_context') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON pending_role_assignment_suspensions
            TO app_identity_context;
    END IF;
END
$$;
