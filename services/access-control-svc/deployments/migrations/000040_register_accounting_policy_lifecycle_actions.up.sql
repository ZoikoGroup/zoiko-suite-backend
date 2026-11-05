-- Migration 000040: register the REF-07 accounting-policy lifecycle actions (ZS-SVC-I-001 s4.17), enforced by policy-svc.
-- Approving, activating and suspending an accounting policy are three separate acts, so they are three permissions: "activation requires
-- designated accounting authority" and "emergency suspension separately authorized". policy-svc honours the emergency suspension only under a
-- live break-glass session, so it is meant to be reached through a session's requested actions rather than granted to a standing role. Who
-- holds the others is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ACCOUNTING_POLICY_APPROVE',           'LEGACY', 'HIGH',     'Enforced by policy-svc: approve an accounting policy version, or review an emergency suspension of one; never its author'),
    ('ACCOUNTING_POLICY_ACTIVATE',          'LEGACY', 'HIGH',     'Enforced by policy-svc: activate or supersede an accounting policy version (the designated accounting authority); never its author'),
    ('ACCOUNTING_POLICY_EMERGENCY_SUSPEND', 'LEGACY', 'CRITICAL', 'Enforced by policy-svc: emergency-suspend an accounting policy version, honoured only under a live break-glass session')
ON CONFLICT (action_name) DO NOTHING;
