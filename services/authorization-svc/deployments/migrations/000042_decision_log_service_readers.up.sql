-- 000042: production grants for the governance decision log's two service
-- readers (tracker AC-2 / D2-2.F07, 11 Nov 2026).
--
-- contract-lifecycle-svc (contract activation verifies its approving decision)
-- and decision-support-svc (precedent sample) read governance-decision-log-svc
-- over mTLS as their own principals, svc-contract-lifecycle and
-- svc-decision-support. The decision log authorizes each read on
-- GOVERNANCE_DECISION_READ, so each principal needs that action in every tenant
-- it reads for. Until now only deployments/scripts/seed-demo-rbac.ps1 granted
-- it, in the demo tenant only: a production tenant refused every activation
-- (the read answered 403 and contract-lifecycle-svc failed closed).
--
-- Roles are tenant-owned here (FindGrantedActionsScoped matches r.tenant_id to
-- the request's tenant), so the grant is per tenant:
--   * every tenant that exists now is provisioned below;
--   * a tenant created later is provisioned when its first role is created
--     (AFTER INSERT trigger on roles): onboarding always creates roles.
-- Each tenant is provisioned ONCE. If the role SVC_DECISION_LOG_READER already
-- exists in a tenant (provisioned before, or created by an administrator or the
-- dev seed) nothing is added, so a tenant administrator who later deactivates
-- the role or ends an assignment is never overridden.
--
-- What is granted is read only: one role, one bundle LEDGER_READ =
-- ["GOVERNANCE_DECISION_READ"], tenant-wide assignments (legal_entity_id NULL)
-- for the two principals. Writers (svc-policy, svc-workflow) are not covered
-- here. Every row is written under its own tenant, so the FORCE RLS WITH CHECK
-- on roles, permission_bundles and principal_role_assignments holds for a
-- non-superuser migration role too; the history and iam.assignment.granted
-- events are emitted by the existing triggers like any other grant.

BEGIN;

CREATE OR REPLACE FUNCTION authz_grant_decision_log_readers(p_tenant UUID) RETURNS VOID
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
    VALUES (p_tenant, 'SVC_DECISION_LOG_READER',
            'Decision-log service readers (contract-lifecycle-svc, decision-support-svc)',
            'TENANT', 'migration:000042')
    ON CONFLICT (tenant_id, role_code) DO NOTHING
    RETURNING role_id INTO v_role;

    IF v_role IS NULL THEN
        RETURN; -- already provisioned in this tenant: never re-grant
    END IF;

    INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions)
    VALUES (v_role, 'LEDGER_READ', '["GOVERNANCE_DECISION_READ"]'::jsonb);

    INSERT INTO principal_role_assignments
        (principal_id, role_id, legal_entity_id, effective_from, assigned_by,
         approval_status, approved_by, approval_reference, approval_decided_at)
    SELECT p.principal, v_role, NULL, now(), 'migration:000042',
           'APPROVED', 'migration:000042', 'AC-2 / D2-2.F07', now()
    FROM (VALUES ('svc-contract-lifecycle'), ('svc-decision-support')) AS p(principal);
END;
$$;

COMMENT ON FUNCTION authz_grant_decision_log_readers(UUID) IS
    'Provisions GOVERNANCE_DECISION_READ for svc-contract-lifecycle and svc-decision-support in one tenant, once (000042).';

CREATE OR REPLACE FUNCTION authz_roles_grant_decision_log_readers() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM authz_grant_decision_log_readers(NEW.tenant_id);
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS roles_decision_log_readers ON roles;
CREATE TRIGGER roles_decision_log_readers
    AFTER INSERT ON roles
    FOR EACH ROW
    WHEN (NEW.role_code <> 'SVC_DECISION_LOG_READER')
    EXECUTE FUNCTION authz_roles_grant_decision_log_readers();

-- Every tenant that exists now. platform_scope only widens what can be READ;
-- each write is made under its own tenant by the function above.
SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000042', true);
SELECT set_config('app.reason', 'AC-2: decision-log service readers (D2-2.F07)', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT DISTINCT tenant_id FROM roles ORDER BY tenant_id LOOP
        PERFORM authz_grant_decision_log_readers(t);
    END LOOP;
END;
$$;

COMMIT;
