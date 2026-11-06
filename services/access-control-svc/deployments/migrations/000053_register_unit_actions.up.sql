-- Migration 000053: register the REF-09 unit-of-measure actions (ZS-SVC-I-001 s4.19), enforced by
-- tenant-entity-registry-svc. Defining dimensions and units and proposing a conversion are steward work (STANDARD).
-- Approving a conversion and retiring a unit change what every quantity-to-money calculation computes or refuses, so those
-- are HIGH. tenant-entity-registry-svc additionally refuses a non-standard conversion approved by the principal who
-- proposed it, whoever holds the permission. Who holds these is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('UNIT_DIMENSION_CREATE',   'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: define a quantity dimension and its base unit'),
    ('UNIT_CREATE',             'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: create a unit of measure, as a draft'),
    ('UNIT_PROPOSE_CONVERSION', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose or supersede a unit conversion; a non-standard one waits for independent approval'),
    ('UNIT_APPROVE_CONVERSION', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: approve a pending conversion; never by the principal who proposed it'),
    ('UNIT_RETIRE',             'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: retire a unit, rejecting its new use')
ON CONFLICT (action_name) DO NOTHING;
