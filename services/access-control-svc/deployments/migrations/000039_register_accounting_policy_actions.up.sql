-- Migration 000039: register the REF-07 accounting-policy actions (ZS-SVC-I-001 s4.17), enforced by policy-svc.
-- Drafting an accounting policy is the author's act. Approving it, activating it (the designated accounting authority) and the emergency
-- suspension are separate permissions, registered with the commands that use them. Who holds them is a role-design decision, so no role
-- template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ACCOUNTING_POLICY_DRAFT', 'LEGACY', 'STANDARD', 'Enforced by policy-svc: draft a version of an accounting policy for a book, jurisdiction and policy domain')
ON CONFLICT (action_name) DO NOTHING;
