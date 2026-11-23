-- Migration 000036: register the ORG-06 assignment suspension actions (ZS-SVC-I-001 s4.6), enforced by authorization-svc (its migration 000037).
-- Suspending withholds access and resuming restores it, so both are HIGH. They are platform administration like the other iam.assignment.* actions,
-- so they join the protected catalogue: a tenant bundle may not grant them. Who holds either is a role-design decision for Security leadership,
-- so no role template is created here.

INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('iam.assignment.suspend', 'Suspend role assignments without ending them', 'platform_admin'),
    ('iam.assignment.resume',  'Resume suspended role assignments',            'platform_admin')
ON CONFLICT (action_name) DO UPDATE SET
    description = EXCLUDED.description,
    category    = EXCLUDED.category,
    active_flag = TRUE,
    updated_at  = now();

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('iam.assignment.suspend', 'TAXONOMY', 'HIGH', 'Suspend role assignments without ending them'),
    ('iam.assignment.resume',  'TAXONOMY', 'HIGH', 'Resume suspended role assignments')
ON CONFLICT (action_name) DO NOTHING;
