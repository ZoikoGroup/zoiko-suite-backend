-- Migration 000044: register the ORG-07 / MDM-01 party actions (ZS-SVC-I-001 s4.7), enforced by tenant-entity-registry-svc.
-- Creating a party, amending its profile, giving it a role and linking an identifier are STANDARD: they are
-- governed by evidence, matching and stewardship cases, not by a second approver. Revealing a sensitive identifier
-- (a tax or national id) is HIGH: reads are masked by default. Who holds these is a role-design decision, so no
-- role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('PARTY_CREATE',            'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: create a party through the match-first create flow'),
    ('PARTY_AMEND',             'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose a sourced fact about a party; never overwrites a verified fact'),
    ('PARTY_ROLE_MANAGE',       'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: add or remove a party role'),
    ('PARTY_IDENTIFIER_LINK',   'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: link an external identifier to a party'),
    ('PARTY_IDENTIFIER_REVEAL', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: read the sensitive identifiers of a party unmasked')
ON CONFLICT (action_name) DO NOTHING;
