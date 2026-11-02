-- Migration 000032: register the approval action of the ORG-05 maker-checker (ZS-SVC-I-001 s4.5). A reparent that carries cost/profit-center
-- mappings is proposed with ORG_UNIT_REPARENT and applied only when a DIFFERENT principal holding this action approves it; org-structure-svc
-- refuses self-approval and the database enforces it as well. HIGH tier: it releases a change to financial structure. Who holds it is a
-- role-design decision for Finance and Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ORG_UNIT_APPROVE_STRUCTURE', 'LEGACY', 'HIGH', 'Enforced by org-structure-svc: approve or reject a proposed org-unit reparent that carries cost/profit-center mappings (never the person who proposed it)')
ON CONFLICT (action_name) DO NOTHING;
