-- 000043 down: stop provisioning new tenants and remove what 000043 granted.
-- Each assignment is ended first (iam.assignment.revoked is emitted), then the
-- rows 000043 created are removed, so a later up provisions those tenants
-- again. Roles an administrator or the dev seed created are left alone.

BEGIN;

DROP TRIGGER IF EXISTS roles_platform_service_identities ON roles;
DROP FUNCTION IF EXISTS authz_roles_grant_platform_service_identities();
DROP FUNCTION IF EXISTS authz_grant_platform_service_identities(UUID);
DROP FUNCTION IF EXISTS authz_provision_service_role(UUID, TEXT, TEXT, TEXT, JSONB, TEXT[], TEXT);

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000043', true);
SELECT set_config('app.reason', 'revert 000043', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT DISTINCT tenant_id FROM roles WHERE created_by_principal_id = 'migration:000043' LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE principal_role_assignments a
           SET effective_to = now()
          FROM roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.created_by_principal_id = 'migration:000043'
           AND a.assigned_by = 'migration:000043'
           AND (a.effective_to IS NULL OR a.effective_to > now());
        DELETE FROM principal_role_assignments a
         USING roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.created_by_principal_id = 'migration:000043' AND a.assigned_by = 'migration:000043';
        DELETE FROM permission_bundles pb
         USING roles r
         WHERE r.role_id = pb.role_id AND r.tenant_id = t AND r.created_by_principal_id = 'migration:000043';
        DELETE FROM roles WHERE tenant_id = t AND created_by_principal_id = 'migration:000043';
    END LOOP;
END;
$$;

COMMIT;
