-- Migration 000054: register the MDM-04 reference release actions (ZS-SVC-U-001 s16), enforced by
-- tenant-entity-registry-svc. Defining a set and staging a release are steward work (STANDARD). Publishing a release changes
-- what every consumer reads a code to mean, so it is HIGH, and tenant-entity-registry-svc additionally refuses a release
-- published by the principal who staged it, whoever holds the permission. Who holds these is a role-design decision, so no
-- role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('REFERENCE_CREATE_SET',      'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: define a governed reference set'),
    ('REFERENCE_STAGE_RELEASE',   'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: stage a reference release; it governs nothing until published'),
    ('REFERENCE_PUBLISH_RELEASE', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: publish a staged reference release; never by the principal who staged it')
ON CONFLICT (action_name) DO NOTHING;
