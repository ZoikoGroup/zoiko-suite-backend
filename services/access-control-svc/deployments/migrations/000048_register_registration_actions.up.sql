-- Migration 000048: register the ORG-09 registration actions (ZS-SVC-I-001 s4.9), enforced by
-- tenant-entity-registry-svc. A registration is evidence other domains rely on: recording one is STANDARD; verifying,
-- renewing, suspending or revoking one changes what can be treated as in force, so those are HIGH. Revealing a
-- sensitive (tax or payroll) number is its own HIGH permission. tenant-entity-registry-svc additionally refuses a
-- material registration (permit, licence, regulated) verified by its creator or renewed by whoever last made it valid,
-- whoever holds the permission. Who holds these is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('REGISTRATION_CREATE',        'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: record a registration or licence, unverified'),
    ('REGISTRATION_VERIFY',        'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: verify a registration on evidence; a material one never by its creator'),
    ('REGISTRATION_RENEW',         'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: renew a registration on evidence; a material one never by whoever last made it valid'),
    ('REGISTRATION_SUSPEND',       'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: suspend a registration'),
    ('REGISTRATION_EXPIRE',        'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: record the expiry of a registration'),
    ('REGISTRATION_REVOKE',        'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: record the revocation of a registration'),
    ('REGISTRATION_NUMBER_REVEAL', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: read an unmasked sensitive registration number')
ON CONFLICT (action_name) DO NOTHING;
