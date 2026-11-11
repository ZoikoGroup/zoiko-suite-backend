-- Migration 000046: register the ORG-08 address actions (ZS-SVC-I-001 s4.8), enforced by tenant-entity-registry-svc.
-- An address is master data other domains print on invoices and file with authorities, so changing one is a
-- governed act, but none of these decides a legal fact: a REGISTERED address is recorded only from authority
-- evidence whoever holds the permission. Who holds them is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ADDRESS_CREATE', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: record a structured address'),
    ('ADDRESS_AMEND',  'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: add a version to an address; history is kept'),
    ('ADDRESS_VERIFY', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: record a verification observation for an address version'),
    ('ADDRESS_LINK',   'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: use an address for a party in a stated capacity')
ON CONFLICT (action_name) DO NOTHING;
