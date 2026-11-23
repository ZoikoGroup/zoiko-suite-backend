-- Migration 000030: register the action the ORG-05 reparent command of org-structure-svc checks (ZS-SVC-I-001 s4.5). Moving a unit
-- changes the operating hierarchy from an effective date on; the independent approval required when the move affects financial
-- dimensions or approvals is a later step. Who holds the action is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ORG_UNIT_REPARENT', 'LEGACY', 'STANDARD', 'Enforced by org-structure-svc: move an organizational unit under a new parent from an effective date (history is kept; loops are refused)')
ON CONFLICT (action_name) DO NOTHING;
