-- Migration 000025: register the action the ZS-JUR-001 s9 jurisdiction determination of jurisdiction-rules-svc checks. It is a
-- read-only determination from the entity's authorised assignments; it registers nothing and changes no assignment. Who holds it is a
-- role-design decision for Tax and Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('JURISDICTION_DECISION_RESOLVE', 'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: determine which jurisdiction applies to a legal entity from its authorised assignments (a claimed jurisdiction can select among them, never add one)')
ON CONFLICT (action_name) DO NOTHING;
