-- Migration 000047: register the ORG-08 establishment actions (ZS-SVC-I-001 s4.8), enforced by
-- tenant-entity-registry-svc. Recording an establishment is a STANDARD act; activating or closing one states a legal
-- presence that tax and legal consumers rely on, so those are HIGH, and tenant-entity-registry-svc additionally
-- refuses a material establishment (branch, permanent establishment) activated by the principal who proposed it,
-- whoever holds the permission. Who holds these is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ESTABLISHMENT_CREATE',     'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose an establishment of a legal entity'),
    ('ESTABLISHMENT_ACTIVATE',   'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: activate an establishment; a material one never by the principal who proposed it'),
    ('ESTABLISHMENT_DEACTIVATE', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: deactivate an establishment, keeping its history'),
    ('ESTABLISHMENT_CLOSE',      'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: close an establishment permanently')
ON CONFLICT (action_name) DO NOTHING;
