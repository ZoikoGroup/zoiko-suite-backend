-- Migration 000029: register the actions the ORG-05 org-unit commands of org-structure-svc check (ZS-SVC-I-001 s4.5): create a unit,
-- view units and their hierarchy, activate a draft unit, and deactivate an active one. Who holds them is a role-design decision for the
-- organization master administrators, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ORG_UNIT_CREATE',     'LEGACY', 'STANDARD', 'Enforced by org-structure-svc: create an organizational unit in DRAFT within a legal entity'),
    ('ORG_UNIT_VIEW',       'LEGACY', 'STANDARD', 'Enforced by org-structure-svc: read organizational units and their hierarchy, current or as of a date'),
    ('ORG_UNIT_ACTIVATE',   'LEGACY', 'STANDARD', 'Enforced by org-structure-svc: activate a draft organizational unit whose parent is active'),
    ('ORG_UNIT_DEACTIVATE', 'LEGACY', 'STANDARD', 'Enforced by org-structure-svc: deactivate an organizational unit that has no active child units')
ON CONFLICT (action_name) DO NOTHING;
