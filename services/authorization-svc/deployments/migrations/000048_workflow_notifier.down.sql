-- 000048 down: restore the 000046 provisioning function and remove what 000048
-- granted (assignments ended first, so iam.assignment.revoked is emitted, then
-- the rows 000048 created are removed).

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
    PERFORM authz_provision_service_role(p_tenant, 'SVC_OBLIGATIONS', 'obligations-svc clock worker',
        'CLOCK_WORKER', '["OBLIGATION_CALCULATION_CALCULATE", "CASE_OPEN"]'::jsonb, ARRAY['svc-obligations'], 'migration:000044');
    PERFORM authz_provision_service_role(p_tenant, 'IDENTITY_PROVISIONING_OPERATOR', 'identity-context-svc SCIM provisioning operator',
        'IDENTITY_PROVISIONING', '["ROLE_MANAGE"]'::jsonb, ARRAY['svc-identity-context'], 'migration:000045');
    PERFORM authz_provision_service_role(p_tenant, 'SVC_AUTHORIZATION_LEDGER', 'authorization-svc decision-log relay',
        'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, ARRAY['svc-authorization'], 'migration:000046');
    PERFORM authz_provision_service_role(p_tenant, 'SVC_JURISDICTION_LEDGER', 'jurisdiction-rules-svc decision-log relay',
        'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, ARRAY['svc-jurisdiction-rules'], 'migration:000046');
    PERFORM authz_provision_service_role(p_tenant, 'SVC_OBLIGATIONS_LEDGER', 'obligations-svc decision-log relay',
        'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, ARRAY['svc-obligations'], 'migration:000046');
    PERFORM authz_provision_service_role(p_tenant, 'SVC_EVIDENCE_LEDGER', 'evidence-requirements-svc decision-log relay',
        'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, ARRAY['svc-evidence-requirements'], 'migration:000046');
END;
$$;

COMMENT ON FUNCTION authz_grant_platform_service_identities(UUID) IS
    'Provisions the platform service identities in one tenant, once per role (000043, 000044 svc-obligations, 000045 svc-identity-context, 000046 decision-log writers).';

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000048', true);
SELECT set_config('app.reason', 'revert 000048', true);
DO $$
DECLARE
    t UUID;
BEGIN
    FOR t IN SELECT DISTINCT tenant_id FROM roles WHERE created_by_principal_id = 'migration:000048' LOOP
        PERFORM set_config('app.tenant_id', t::text, true);
        UPDATE principal_role_assignments a
           SET effective_to = now()
          FROM roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.created_by_principal_id = 'migration:000048'
           AND a.assigned_by = 'migration:000048'
           AND (a.effective_to IS NULL OR a.effective_to > now());
        DELETE FROM principal_role_assignments a
         USING roles r
         WHERE r.role_id = a.role_id AND r.tenant_id = t
           AND r.created_by_principal_id = 'migration:000048' AND a.assigned_by = 'migration:000048';
        DELETE FROM permission_bundles pb
         USING roles r
         WHERE r.role_id = pb.role_id AND r.tenant_id = t AND r.created_by_principal_id = 'migration:000048';
        DELETE FROM roles WHERE tenant_id = t AND created_by_principal_id = 'migration:000048';
    END LOOP;
END;
$$;

COMMIT;
