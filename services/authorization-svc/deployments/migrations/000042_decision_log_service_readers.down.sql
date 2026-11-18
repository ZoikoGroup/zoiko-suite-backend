-- 000042 down: stop provisioning new tenants and remove what 000042 granted.
-- Each assignment is ended first, so iam.assignment.revoked is emitted like
-- for any revocation, then the rows 000042 created (assignments, bundle,
-- role, all marked migration:000042) are removed, so a later up provisions
-- those tenants again. A role an administrator or the dev seed created is
-- left alone.

BEGIN;

DROP TRIGGER IF EXISTS roles_decision_log_readers ON roles;
DROP FUNCTION IF EXISTS authz_roles_grant_decision_log_readers();
DROP FUNCTION IF EXISTS authz_grant_decision_log_readers(UUID);

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000042', true);
SELECT set_config('app.reason', 'revert 000042', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT tenant_id FROM roles
              WHERE role_code = 'SVC_DECISION_LOG_READER' AND created_by_principal_id = 'migration:000042'
    LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE principal_role_assignments a
           SET effective_to = now()
          FROM roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.role_code = 'SVC_DECISION_LOG_READER'
           AND a.assigned_by = 'migration:000042'
           AND (a.effective_to IS NULL OR a.effective_to > now());
        DELETE FROM principal_role_assignments a
         USING roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.role_code = 'SVC_DECISION_LOG_READER' AND a.assigned_by = 'migration:000042';
        DELETE FROM permission_bundles pb
         USING roles r
         WHERE r.role_id = pb.role_id AND r.tenant_id = t
           AND r.role_code = 'SVC_DECISION_LOG_READER' AND r.created_by_principal_id = 'migration:000042';
        DELETE FROM roles
         WHERE tenant_id = t AND role_code = 'SVC_DECISION_LOG_READER'
           AND created_by_principal_id = 'migration:000042';
    END LOOP;
END;
$$;

COMMIT;
