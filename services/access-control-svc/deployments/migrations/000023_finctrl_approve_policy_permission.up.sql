-- Migration 000023: register FINCTRL_APPROVE_POLICY (financial-control-svc
-- SPEC_DEVIATIONS A4/000010: tolerance and materiality policies now require
-- a second, distinct authenticated approver instead of a caller-declared
-- approved_by field. Without this row no role can hold the action and
-- every approve request answers 403.
--
-- LEGACY naming and STANDARD risk tier, matching every sibling FINCTRL_*
-- action (000008); the service itself also refuses an approver who created
-- the policy being approved, whatever is granted here, the same
-- independence guard FINCTRL_APPROVE_RULE's callers already rely on.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('FINCTRL_APPROVE_POLICY', 'LEGACY', 'STANDARD', '')
ON CONFLICT (action_name) DO NOTHING;
