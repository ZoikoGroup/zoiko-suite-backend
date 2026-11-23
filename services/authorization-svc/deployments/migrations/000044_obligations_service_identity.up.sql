-- 000044: production grant for obligations-svc's service identity (tracker
-- D2-4 OB-S8 / OB-S11, 23 Nov 2026), on the 000043 pattern.
--
--   role             principal         actions
--   SVC_OBLIGATIONS  svc-obligations   OBLIGATION_CALCULATION_CALCULATE, CASE_OPEN
--
-- obligations-svc acts as svc-obligations outside any request in two places:
-- the calendar consumer asks jurisdiction-rules-svc to recalculate every live
-- clock on a corrected calendar (R-001 s7.3, NP-14), and the escalation relay
-- opens the workflow-svc case an OPEN_CASE milestone requested (R-001 s7.4).
-- CASE_OPEN is the action workflow-svc's case commands check (WF-S2); it is
-- registered in access-control by the workflow-svc work.
--
-- Same rules as 000043: every existing tenant now, a new tenant when its first
-- role is created (the 000043 trigger calls the function this migration
-- extends), each role once per tenant, never re-granted.

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
END;
$$;

COMMENT ON FUNCTION authz_grant_platform_service_identities(UUID) IS
    'Provisions the platform service identities in one tenant, once per role (000043, 000044: svc-obligations).';

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000044', true);
SELECT set_config('app.reason', 'OB-S8/OB-S11: obligations-svc service identity grant', true);
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
