-- Migration 000018: the permissions authorization-svc's session controls check
-- (its migration 000030; ZS-IAM-001 §13-15).
--
--   iam.pam.eligible          held by the SUBJECT of a JIT session (§13 eligibility)
--   iam.pam.approve           independent approval of a high-impact JIT session
--   iam.break_glass.eligible  held by the SUBJECT of a break-glass session (§14)
--   iam.session.review        retrospective review of ended JIT / break-glass sessions
--
-- Registered like the iam.*.manage session permissions (000007/000008):
-- protected platform-admin and CRITICAL, so a tenant cannot put them in its
-- own roles and an assignment of one needs independent approval.

INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('iam.pam.eligible',         'Eligible to request JIT privileged sessions', 'platform_admin'),
    ('iam.pam.approve',          'Approve high-impact JIT privileged sessions', 'platform_admin'),
    ('iam.break_glass.eligible', 'Eligible for break-glass emergency sessions', 'platform_admin'),
    ('iam.session.review',       'Review ended privileged and break-glass sessions', 'platform_admin')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('iam.pam.eligible',         'TAXONOMY', 'CRITICAL', 'Eligible to request JIT privileged sessions'),
    ('iam.pam.approve',          'TAXONOMY', 'CRITICAL', 'Approve high-impact JIT privileged sessions'),
    ('iam.break_glass.eligible', 'TAXONOMY', 'CRITICAL', 'Eligible for break-glass emergency sessions'),
    ('iam.session.review',       'TAXONOMY', 'CRITICAL', 'Review ended privileged and break-glass sessions')
ON CONFLICT (action_name) DO NOTHING;
