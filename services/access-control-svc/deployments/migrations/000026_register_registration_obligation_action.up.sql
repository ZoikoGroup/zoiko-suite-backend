-- Migration 000026: register the action the ZS-JUR-001 s15/s16 registration-driven applicability of jurisdiction-rules-svc checks. It is a
-- read-only answer (which obligations apply for an entity's registrations, and when each is due); it registers nothing, stores no obligation
-- and changes no assignment. Who holds it is a role-design decision for Tax and Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('REGISTRATION_OBLIGATION_RESOLVE', 'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: list the obligations that apply for an entity''s registrations and derive each due date (obligation instances and overdue tracking stay in obligations-svc)')
ON CONFLICT (action_name) DO NOTHING;
