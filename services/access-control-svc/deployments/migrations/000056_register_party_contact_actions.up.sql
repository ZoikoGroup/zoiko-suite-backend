-- Migration 000056: register the party contact point actions (ZS-SVC-U-001 MDM-01), enforced by tenant-entity-registry-svc.
-- A contact point is an address a notifier may use for a party of the same tenant. Proposing and retiring one are steward work
-- (STANDARD). Verifying one makes it usable for delivery, so it is HIGH, and tenant-entity-registry-svc additionally refuses a
-- verification by the principal who proposed it, whoever holds the permission. Reading returns the address itself, so it is a
-- separate permission and a notifier is granted it explicitly. Who holds these is a role-design decision, so no role template
-- is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('PARTY_CONTACT_CREATE', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose a contact point (email) for a party'),
    ('PARTY_CONTACT_VERIFY', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: verify a proposed contact point on evidence; never by the principal who proposed it'),
    ('PARTY_CONTACT_RETIRE', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: retire a contact point, finally'),
    ('PARTY_CONTACT_READ',   'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: read the contact points of a party, including the address value')
ON CONFLICT (action_name) DO NOTHING;
