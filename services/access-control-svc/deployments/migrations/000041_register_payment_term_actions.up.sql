-- Migration 000041: register the REF-10 payment-term actions (ZS-SVC-I-001 s4.20), enforced by jurisdiction-rules-svc.
-- Drafting a payment term is the steward's act. Approving, superseding and retiring one change how due dates are worked out for everything
-- that counts from it, so they are HIGH. jurisdiction-rules-svc refuses an approval by the author of the version, whoever holds the
-- permission. Who holds them is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('PAYMENT_TERM_CREATE',    'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: register a payment term and draft its versions'),
    ('PAYMENT_TERM_APPROVE',   'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: approve a payment term version, putting it in force; never its author'),
    ('PAYMENT_TERM_SUPERSEDE', 'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: approve a payment term version over the active version it replaces; never its author'),
    ('PAYMENT_TERM_RETIRE',    'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: retire a payment term version, withdrawing a draft or ending an active one')
ON CONFLICT (action_name) DO NOTHING;
