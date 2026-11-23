-- 000045: production grant for identity-context-svc's SCIM provisioning
-- (tracker D1-1 IC-15, 23 Nov 2026), on the 000043 pattern.
--
--   role                             principal              actions
--   IDENTITY_PROVISIONING_OPERATOR   svc-identity-context   ROLE_MANAGE
--
-- identity-context-svc's federation provisioning pipeline creates the
-- assignments a SCIM mapping names by calling access-control-svc, which asks
-- this service whether svc-identity-context may ROLE_MANAGE. The grant was
-- first written as access-control-svc migration 000061 against tables that
-- exist only here (roles, permission_bundles, principal_role_assignments), so
-- it could not apply anywhere; 000061 is now a no-op that points here.
-- The role code is the dev seed's, so the demo tenant (which already holds
-- it) is left as it is.

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
END;
$$;

COMMENT ON FUNCTION authz_grant_platform_service_identities(UUID) IS
    'Provisions the platform service identities in one tenant, once per role (000043, 000044 svc-obligations, 000045 svc-identity-context).';

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000045', true);
SELECT set_config('app.reason', 'IC-15: identity-context-svc SCIM provisioning operator grant', true);
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
