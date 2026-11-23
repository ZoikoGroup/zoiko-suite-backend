-- 000044 down: restore the 000043 provisioning function and remove what
-- 000044 granted (assignments ended first, so iam.assignment.revoked is
-- emitted, then the rows 000044 created are removed).

BEGIN;

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

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000044', true);
SELECT set_config('app.reason', 'revert 000044', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT DISTINCT tenant_id FROM roles WHERE created_by_principal_id = 'migration:000044' LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE principal_role_assignments a
           SET effective_to = now()
          FROM roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.created_by_principal_id = 'migration:000044'
           AND a.assigned_by = 'migration:000044'
           AND (a.effective_to IS NULL OR a.effective_to > now());
        DELETE FROM principal_role_assignments a
         USING roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.created_by_principal_id = 'migration:000044' AND a.assigned_by = 'migration:000044';
        DELETE FROM permission_bundles pb
         USING roles r
         WHERE r.role_id = pb.role_id AND r.tenant_id = t AND r.created_by_principal_id = 'migration:000044';
        DELETE FROM roles WHERE tenant_id = t AND created_by_principal_id = 'migration:000044';
    END LOOP;
END;
$$;

COMMIT;
