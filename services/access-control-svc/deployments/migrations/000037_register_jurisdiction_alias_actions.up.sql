-- Migration 000037: register the REF-01 alias actions (ZS-SVC-I-001 s4.11), enforced by jurisdiction-rules-svc.
-- AmendJurisdictionAlias changes what an external code means, which every consumer of code resolution then relies on, so both are
-- reference-data steward actions rather than ordinary operator ones. Who holds them is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('JURISDICTION_ALIAS_AMEND', 'LEGACY', 'HIGH', 'Enforced by jurisdiction-rules-svc: map an external code to a jurisdiction, or move it to another'),
    ('JURISDICTION_ALIAS_END',   'LEGACY', 'HIGH', 'Enforced by jurisdiction-rules-svc: end an external code mapping')
ON CONFLICT (action_name) DO NOTHING;
