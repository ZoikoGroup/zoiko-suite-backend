-- 000046: production grants for the four new governance-ledger writers
-- (tracker "Assignment 25 Nov 2026: automatic decision capture", step DL-1),
-- on the 000043 pattern.
--
--   role                        principal                   actions
--   SVC_AUTHORIZATION_LEDGER    svc-authorization           GOVERNANCE_DECISION_RECORD
--   SVC_JURISDICTION_LEDGER     svc-jurisdiction-rules      GOVERNANCE_DECISION_RECORD
--   SVC_OBLIGATIONS_LEDGER      svc-obligations             GOVERNANCE_DECISION_RECORD
--   SVC_EVIDENCE_LEDGER         svc-evidence-requirements   GOVERNANCE_DECISION_RECORD
--
-- Doc 01 s7: every governance decision is itself evidence. Each of these
-- engines now relays its own verdicts to governance-decision-log-svc from a
-- transactional outbox, over the ledger's mTLS listener with its own workload
-- certificate, as its service principal (decision D-DL-2: only service
-- identities hold GOVERNANCE_DECISION_RECORD). svc-obligations already holds
-- the 000044 clock-worker role; the ledger grant is a role of its own, so a
-- tenant provisioned by 000044 receives it too (roles are provisioned once
-- each, never re-granted).
--
-- Same rules as 000043: every existing tenant now, a new tenant when its first
-- role is created (the 000043 trigger calls the function this migration
-- extends), each role once per tenant, never re-granted, tenant-wide
-- assignments (legal_entity_id NULL).

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
SELECT set_config('app.actor_id', 'migration:000046', true);
SELECT set_config('app.reason', 'DL-1: decision-log writer grants', true);
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
