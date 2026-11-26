-- 000048: workflow-svc's notice sender (26 Nov 2026), on the 000043 pattern.
--
--   role                    principal       actions
--   SVC_WORKFLOW_NOTIFIER   svc-workflow    NOTIFICATION_SEND, IDENTITY_PRINCIPAL_READ
--
-- Doc 03 s9.7: notification-svc handles governed notifications for workflows,
-- escalations and approvals. workflow-svc now tells approvers a request waits
-- on them and requesters how it was decided: each transition writes its notices
-- to workflow-svc's outbox, and a relay sends them to notification-svc's
-- POST /v1/notifications as svc-workflow. notification-svc authorizes every
-- send as NOTIFICATION_SEND for the sending principal, and resolves an EMAIL
-- recipient's address from identity-context-svc's GET /v1/principals/{id} AS
-- THAT SAME SENDER, which identity-context-svc guards with
-- IDENTITY_PRINCIPAL_READ. Without the read, every workflow email waits
-- RECIPIENT_UNRESOLVED (verified live 26 Nov 2026: authorization DENY
-- no_grant); the in-app notice is unaffected. The grant is a role of its own
-- (not added to SVC_WORKFLOW_LEDGER), so a tenant already provisioned by 000043
-- receives it too: roles are provisioned once each, never re-granted.
--
-- Same rules as 000043: every existing tenant now, a new tenant when its first
-- role is created (the 000043 trigger calls the function this migration
-- extends), tenant-wide assignments (legal_entity_id NULL).

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
    PERFORM authz_provision_service_role(p_tenant, 'SVC_WORKFLOW_NOTIFIER', 'workflow-svc approver and requester notices',
        'NOTIFY_SEND', '["NOTIFICATION_SEND", "IDENTITY_PRINCIPAL_READ"]'::jsonb, ARRAY['svc-workflow'], 'migration:000048');
END;
$$;

COMMENT ON FUNCTION authz_grant_platform_service_identities(UUID) IS
    'Provisions the platform service identities in one tenant, once per role (000043, 000044 svc-obligations, 000045 svc-identity-context, 000046 decision-log writers, 000048 workflow notices).';

SELECT set_config('app.platform_scope', 'true', true);
SELECT set_config('app.actor_id', 'migration:000048', true);
SELECT set_config('app.reason', 'workflow notices: svc-workflow sends notices and resolves recipients', true);
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
