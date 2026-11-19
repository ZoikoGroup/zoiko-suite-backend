-- Migration 000061: grant identity-context-svc the operator role it needs
-- for live SCIM provisioning (IC-15 / S1-B5, 19 Nov 2026).
--
-- identity-context-svc's federation provisioning pipeline (internal/federation/provision.go)
-- calls access-control-svc to create assignments for mapped roles. It acts as
-- svc-identity-context and must hold ROLE_MANAGE on the legal entities its
-- mappings name. Until now this was only granted by the demo seed
-- (deployments/scripts/seed-demo-rbac.ps1) in the demo tenant, so provisioning
-- in any other tenant failed with 403 provisioning_forbidden.
--
-- This migration provisions a tenant-owned role IDENTITY_PROVISIONING_OPERATOR
-- carrying ROLE_MANAGE, assigned to svc-identity-context, in every tenant
-- (existing and future via trigger), the same pattern as 000043 in
-- authorization-svc.

BEGIN;

-- Provisions one service role in one tenant, once. Rows are written under
-- their own tenant, so FORCE RLS WITH CHECK holds for any migration role.
CREATE OR REPLACE FUNCTION acs_provision_identity_provisioning_role(
    p_tenant UUID, p_by TEXT)
RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    v_role UUID;
BEGIN
    IF p_tenant IS NULL THEN
        RETURN;
    END IF;
    IF current_setting('app.tenant_id', true) IS DISTINCT FROM p_tenant::text THEN
        PERFORM set_config('app.tenant_id', p_tenant::text, true);
    END IF;

    INSERT INTO roles (tenant_id, role_code, role_name, role_scope_type, created_by_principal_id)
    VALUES (p_tenant, 'IDENTITY_PROVISIONING_OPERATOR', 'identity-context-svc SCIM provisioning operator', 'LEGAL_ENTITY', p_by)
    ON CONFLICT (tenant_id, role_code) DO NOTHING
    RETURNING role_id INTO v_role;

    IF v_role IS NULL THEN
        RETURN; -- already provisioned in this tenant: never re-grant
    END IF;

    INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions)
    VALUES (v_role, 'IDENTITY_PROVISIONING', '["ROLE_MANAGE"]'::jsonb);

    INSERT INTO principal_role_assignments
        (principal_id, role_id, legal_entity_id, effective_from, assigned_by,
         approval_status, approved_by, approval_reference, approval_decided_at)
    VALUES ('svc-identity-context', v_role, NULL, now(), p_by, 'APPROVED', p_by, 'IC-15', now());
END;
$$;

CREATE OR REPLACE FUNCTION acs_grant_identity_provisioning_operator(p_tenant UUID) RETURNS VOID
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM acs_provision_identity_provisioning_role(p_tenant, 'migration:000061');
END;
$$;

COMMENT ON FUNCTION acs_grant_identity_provisioning_operator(UUID) IS
    'Provisions IDENTITY_PROVISIONING_OPERATOR with ROLE_MANAGE for svc-identity-context in one tenant, once per role (000061).';

CREATE OR REPLACE FUNCTION acs_roles_grant_identity_provisioning_operator() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM acs_grant_identity_provisioning_operator(NEW.tenant_id);
    RETURN NULL;
END;
$$;

-- Fires for a role somebody created, not for the service roles the
-- provisioning functions create themselves.
DROP TRIGGER IF EXISTS roles_identity_provisioning_operator ON roles;
CREATE TRIGGER roles_identity_provisioning_operator
    AFTER INSERT ON roles
    FOR EACH ROW
    WHEN (NEW.created_by_principal_id NOT LIKE 'migration:%')
    EXECUTE FUNCTION acs_roles_grant_identity_provisioning_operator();

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000061', true);
SELECT set_config('app.reason', 'IC-15: identity-context-svc SCIM provisioning operator grant', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT DISTINCT tenant_id FROM roles ORDER BY tenant_id LOOP
        PERFORM acs_grant_identity_provisioning_operator(t);
    END LOOP;
END;
$$;

COMMIT;