-- Migration 000010: revocation tombstones for the role-assignment projection
-- (Group 1 re-audit gap S1-4).
--
-- iam.assignment.granted and iam.assignment.revoked can arrive out of order:
-- they were keyed differently by access-control-svc (fixed there on 7 Oct),
-- they come through a retried consumer, and a replay redelivers old grants.
-- When the revoke arrived first, EndRoleAssignment updated 0 rows; the late
-- grant then inserted the projection with effective_to = infinity, and the
-- next resolve framed the revoked role again.
--
-- A revocation is now recorded here even when the projection holds no row,
-- and a grant can never open an assignment past its recorded revocation.

CREATE TABLE IF NOT EXISTS revoked_role_assignments (
    tenant_id     VARCHAR(255) NOT NULL,
    assignment_id VARCHAR(255) NOT NULL,
    revoked_at    TIMESTAMP WITH TIME ZONE NOT NULL,
    recorded_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, assignment_id)
);

ALTER TABLE revoked_role_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE revoked_role_assignments FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON revoked_role_assignments;
CREATE POLICY tenant_isolation_policy ON revoked_role_assignments
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
