-- 000043: production grants for the platform's other service identities
-- (tracker AC-2, 11 Nov 2026), the same way 000042 grants the decision log's
-- readers.
--
--   role                 principal            actions
--   SVC_POLICY_LEDGER    svc-policy           GOVERNANCE_DECISION_RECORD
--   SVC_WORKFLOW_LEDGER  svc-workflow         GOVERNANCE_DECISION_RECORD
--   SVC_ACCESS_CONTROL   svc-access-control   iam.assignment.revoke, iam.assignment.read
--
-- policy-svc's and workflow-svc's ledger relays write every evaluation and
-- workflow decision to governance-decision-log-svc as their own principals
-- (decision D-02); access-control-svc executes an access review's REVOKE as
-- svc-access-control. Each needs its action in every tenant it acts for, and
-- until now only deployments/scripts/seed-demo-rbac.ps1 granted them, in the
-- demo tenant: in any other tenant evaluations and workflow decisions waited
-- in their outboxes forever and review revocations failed.
--
-- Same rules as 000042: roles are tenant-owned, so every existing tenant is
-- provisioned now and a new tenant when its first role is created (trigger);
-- each role once per tenant, never re-granted, so an administrator's
-- revocation stands; tenant-wide assignments (legal_entity_id NULL) match any
-- entity and the tenant scope the iam.* checks use. The role codes are the dev
-- seed's, so the demo tenant (which already holds them) is left as it is.

BEGIN;

-- Provisions one service role in one tenant, once. Rows are written under
-- their own tenant, so FORCE RLS WITH CHECK holds for any migration role.
CREATE OR REPLACE FUNCTION authz_provision_service_role(
    p_tenant UUID, p_code TEXT, p_name TEXT, p_bundle TEXT, p_actions JSONB, p_principals TEXT[], p_by TEXT)
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
    VALUES (p_tenant, p_code, p_name, 'TENANT', p_by)
    ON CONFLICT (tenant_id, role_code) DO NOTHING
    RETURNING role_id INTO v_role;

    IF v_role IS NULL THEN
        RETURN; -- already provisioned in this tenant: never re-grant
    END IF;

    INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions)
    VALUES (v_role, p_bundle, p_actions);

    INSERT INTO principal_role_assignments
        (principal_id, role_id, legal_entity_id, effective_from, assigned_by,
         approval_status, approved_by, approval_reference, approval_decided_at)
    SELECT p, v_role, NULL, now(), p_by, 'APPROVED', p_by, 'AC-2', now()
    FROM unnest(p_principals) AS p;
END;
$$;

CREATE OR REPLACE FUNCTION authz_grant_platform_service_identities(p_tenant UUID) RETURNS VOID
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM authz_provision_service_role(p_tenant, 'SVC_POLICY_LEDGER', 'policy-svc evidence relay',
        'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, ARRAY['svc-policy'], 'migration:000043');
    PERFORM authz_provision_service_role(p_tenant, 'SVC_WORKFLOW_LEDGER', 'workflow-svc ledger relay',
        'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, ARRAY['svc-workflow'], 'migration:000043');
    PERFORM authz_provision_service_role(p_tenant, 'SVC_ACCESS_CONTROL', 'access-control-svc review executor',
        'REVIEW_REVOKE', '["iam.assignment.revoke", "iam.assignment.read"]'::jsonb, ARRAY['svc-access-control'], 'migration:000043');
END;
$$;

COMMENT ON FUNCTION authz_grant_platform_service_identities(UUID) IS
    'Provisions svc-policy / svc-workflow GOVERNANCE_DECISION_RECORD and svc-access-control iam.assignment.revoke/read in one tenant, once per role (000043).';

CREATE OR REPLACE FUNCTION authz_roles_grant_platform_service_identities() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM authz_grant_platform_service_identities(NEW.tenant_id);
    RETURN NULL;
END;
$$;

-- Fires for a role somebody created, not for the service roles the
-- provisioning functions create themselves.
DROP TRIGGER IF EXISTS roles_platform_service_identities ON roles;
CREATE TRIGGER roles_platform_service_identities
    AFTER INSERT ON roles
    FOR EACH ROW
    WHEN (NEW.created_by_principal_id NOT LIKE 'migration:%')
    EXECUTE FUNCTION authz_roles_grant_platform_service_identities();

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000043', true);
SELECT set_config('app.reason', 'AC-2: platform service identity grants', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT DISTINCT tenant_id FROM roles ORDER BY tenant_id LOOP
        PERFORM authz_grant_platform_service_identities(t);
    END LOOP;
END;
$$;

COMMIT;
