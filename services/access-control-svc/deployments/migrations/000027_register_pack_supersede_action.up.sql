-- Migration 000027: register the action the ZS-JUR-001 s23 supersede command of jurisdiction-rules-svc checks. Marking a RELEASED pack version
-- SUPERSEDED takes it out of service for new decisions (historical resolution stays permitted), so it is as consequential as a withdrawal and is
-- refused unless the newer version covers everything the older one served. Who holds it is a role-design decision for Tax and Security leadership,
-- so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('JURISDICTION_PACK_VERSION_SUPERSEDE', 'LEGACY', 'HIGH', 'Enforced by jurisdiction-rules-svc: mark a released pack version superseded by a newer one that covers its period, jurisdictions, rules, obligations and deployments (reason required)')
ON CONFLICT (action_name) DO NOTHING;
