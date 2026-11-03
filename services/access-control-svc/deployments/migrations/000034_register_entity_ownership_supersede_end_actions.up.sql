-- Migration 000034: register the ORG-04 wave 2 actions (ZS-SVC-I-001 s4.4), enforced by tenant-entity-registry-svc.
-- SUPERSEDE proposes new terms for an active relationship; the replacement still needs ENTITY_OWNERSHIP_APPROVE from a different principal.
-- END takes effect immediately, with no second approver, so it is HIGH tier. Who holds either is a role-design decision for Finance and
-- Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ENTITY_OWNERSHIP_SUPERSEDE', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose new terms for an active ownership or control relationship (approved separately)'),
    ('ENTITY_OWNERSHIP_END',       'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: end an active ownership or control relationship, effective immediately')
ON CONFLICT (action_name) DO NOTHING;
