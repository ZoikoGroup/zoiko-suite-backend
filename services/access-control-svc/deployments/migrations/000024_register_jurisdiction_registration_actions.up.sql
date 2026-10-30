-- Migration 000024: register the actions the ZS-JUR-001 s16 commands of jurisdiction-rules-svc check (registration identifier formats, licences,
-- schemes and elections, threshold monitors). These are read-only evaluations of the governed rule in force; none of them registers an entity,
-- stores a registration or contacts an authority. Who holds them is a role-design decision for Tax and Security leadership, so no role template
-- is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('REGISTRATION_IDENTIFIER_VALIDATE', 'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: check a registration identifier''s format against the registration type in force (a format check only; the identifier is never stored)'),
    ('LICENCE_EVALUATE',                 'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: derive a licence''s validity window and status from the licence type in force'),
    ('SCHEME_ELECTION_EVALUATE',         'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: check scheme eligibility and the earliest cancellation date under the lock-in and notice rules (records no election)'),
    ('THRESHOLD_MONITOR_EVALUATE',       'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: evaluate dated observations against a threshold monitor and return the breach and effective dates')
ON CONFLICT (action_name) DO NOTHING;
