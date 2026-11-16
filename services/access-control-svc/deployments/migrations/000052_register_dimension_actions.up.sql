-- Migration 000052: register the REF-08 dimension actions (ZS-SVC-I-001 s4.18), enforced by
-- tenant-entity-registry-svc. Creating types and values, activating them and mapping source objects are steward work
-- (STANDARD). Deactivating a value decides what can no longer take a posting, and approving a hierarchy change decides how
-- reporting and control aggregate, so those are HIGH. tenant-entity-registry-svc additionally refuses a controlled
-- hierarchy change approved by the principal who proposed it, and any hierarchy loop, whoever holds the permission. Who
-- holds these is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('DIMENSION_TYPE_CREATE',        'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: define a financial dimension type'),
    ('DIMENSION_CREATE',             'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: create a dimension value, as a draft'),
    ('DIMENSION_ACTIVATE',           'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: activate a dimension value so it can take postings'),
    ('DIMENSION_DEACTIVATE',         'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: deactivate a dimension value, rejecting new postings; may record a controlled exception'),
    ('DIMENSION_REPARENT',           'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose a hierarchy or label change; a controlled one waits for independent approval'),
    ('DIMENSION_APPROVE_HIERARCHY',  'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: approve a pending hierarchy change; never by the principal who proposed it'),
    ('DIMENSION_MAP_SOURCE',         'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: map an operational source object to a dimension value, by version')
ON CONFLICT (action_name) DO NOTHING;
