-- Migration 000042: register the ORG-01 identity read actions (ZS-SVC-I-001 s4.1), enforced by identity-context-svc.
-- Reading your own identity needs no grant. Reading someone else's subject, links and assurance history is STANDARD;
-- the authentication-event read model says when and how a person signed in and where they were refused, so it is HIGH.
-- Who holds them is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('IDENTITY_SUBJECT_READ',    'LEGACY', 'STANDARD', 'Enforced by identity-context-svc: read another subject identity, its provider links and its assurance history'),
    ('IDENTITY_AUTH_EVENT_READ', 'LEGACY', 'HIGH',     'Enforced by identity-context-svc: read another subject sign-in and second-factor events')
ON CONFLICT (action_name) DO NOTHING;
