-- Migration 000031: register the action the ORG-05 change-manager command of org-structure-svc checks (ZS-SVC-I-001 s4.5). The manager is
-- a reference to an employee resolved against employee-master-svc; the previous manager stays in history. Who holds the action is a
-- role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ORG_UNIT_CHANGE_MANAGER', 'LEGACY', 'STANDARD', 'Enforced by org-structure-svc: name a new manager (an existing employee) for an organizational unit from an effective date; the previous manager stays in history')
ON CONFLICT (action_name) DO NOTHING;
