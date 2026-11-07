-- Migration 000033: register the ORG-04 actions (ZS-SVC-I-001 s4.4, Corporate Group and Ownership), enforced by tenant-entity-registry-svc.
-- PROPOSE files an ownership or control relationship. APPROVE releases it and is HIGH tier because an approved relationship changes the
-- group structure that consolidation consumes. The service refuses approval by the proposer and the database enforces it as well. Who holds
-- APPROVE is a role-design decision for Finance and Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ENTITY_OWNERSHIP_PROPOSE', 'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: propose an ownership or control relationship between two legal entities'),
    ('ENTITY_OWNERSHIP_APPROVE', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: approve or reject a proposed ownership or control relationship (never the proposer)')
ON CONFLICT (action_name) DO NOTHING;
