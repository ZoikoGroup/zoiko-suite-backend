-- Migration 000035: register the ORG-04 read actions (ZS-SVC-I-001 s4.4), enforced by tenant-entity-registry-svc.
-- VIEW returns the group structure. VIEW_SENSITIVE additionally returns the beneficial and control notes attached to a relationship, which
-- s4.4 requires be scoped; without it a caller still gets the structure, with the notes omitted. Who holds either is a role-design decision
-- for Finance, Legal and Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ENTITY_OWNERSHIP_VIEW',           'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: read ownership and control relationships and the group graph'),
    ('ENTITY_OWNERSHIP_VIEW_SENSITIVE', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: read the beneficial and control notes on ownership relationships')
ON CONFLICT (action_name) DO NOTHING;
