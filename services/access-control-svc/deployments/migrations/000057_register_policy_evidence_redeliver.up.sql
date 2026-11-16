-- Migration 000057: register POLICY_EVIDENCE_REDELIVER, enforced by policy-svc.
-- Evaluation evidence the governance ledger refused for good (ledger_status FAILED) stays parked until an operator
-- fixes the cause and asks for it to be sent again (POST /v1/policy-evaluations/{id}/redeliver). Re-sending puts a
-- tenant's evidence back into the governance ledger's queue, so it is HIGH and separate from POLICY_READ. Who holds
-- it is an operations role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('POLICY_EVIDENCE_REDELIVER', 'LEGACY', 'HIGH', 'Enforced by policy-svc: re-queue policy evaluation evidence the governance ledger refused')
ON CONFLICT (action_name) DO NOTHING;
