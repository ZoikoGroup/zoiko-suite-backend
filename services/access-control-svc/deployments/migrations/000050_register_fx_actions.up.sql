-- Migration 000050: register the REF-03 exchange-rate actions (ZS-SVC-I-001 s4.13), enforced by
-- tenant-entity-registry-svc. Ingesting an observation is market-data work (STANDARD). Registering or withdrawing a
-- source, correcting an observation, approving a rate set and closing one decide which rates accounting, tax and
-- reporting may use, so those are HIGH. tenant-entity-registry-svc additionally refuses a manual or outlying rate
-- approved by the principal who ingested it, whoever holds the permission. Who holds these is a role-design decision,
-- so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('FX_SOURCE_MANAGE',         'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: register or withdraw an approved exchange-rate source'),
    ('FX_OBSERVATION_INGEST',    'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: record an exchange-rate observation from an approved source'),
    ('FX_OBSERVATION_SUPERSEDE', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: replace an exchange-rate observation that is not yet in a rate set'),
    ('FX_RATE_SET_APPROVE',      'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: approve a rate set; a manual or outlying rate never by whoever ingested it'),
    ('FX_RATE_SET_CLOSE',        'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: certify a rate set, after which it is immutable')
ON CONFLICT (action_name) DO NOTHING;
