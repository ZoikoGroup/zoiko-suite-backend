-- Migration 000019: register the actions policy-svc's GOV-05 lifecycle,
-- evaluation and read gates check (Governance Platform audit, policy-svc
-- closure 14 Oct 2026, register P-05).
--
-- A bundle may only name registered actions, so until these exist no role can
-- grant them and every one of policy-svc's new gates can only answer DENIED.
--
-- Same naming as policy-svc's existing POLICY_* entries (LEGACY, 000008).
-- Risk tiers follow GOV-05: "emergency suspension privileged" makes both
-- SUSPEND actions HIGH (an assignment of a HIGH action needs independent
-- approval); the platform-wide variants of approve and retire govern every
-- tenant at once, so they are HIGH as well. The rest are STANDARD.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('POLICY_READ',                    'LEGACY', 'STANDARD', 'Enforced by policy-svc: read policies, versions, resolutions and evaluation evidence'),
    ('POLICY_EVALUATE',                'LEGACY', 'STANDARD', 'Enforced by policy-svc: evaluate a policy against an action context'),
    ('POLICY_CREATE_GLOBAL',           'LEGACY', 'HIGH',     'Enforced by policy-svc: create a platform-wide policy family'),
    ('POLICY_VERSION_APPROVE',         'LEGACY', 'STANDARD', 'Enforced by policy-svc: approve a reviewed policy version (author excluded)'),
    ('POLICY_VERSION_APPROVE_GLOBAL',  'LEGACY', 'HIGH',     'Enforced by policy-svc: approve a platform-wide policy version'),
    ('POLICY_VERSION_SUSPEND',         'LEGACY', 'HIGH',     'Enforced by policy-svc: emergency suspension of a live policy version'),
    ('POLICY_VERSION_SUSPEND_GLOBAL',  'LEGACY', 'HIGH',     'Enforced by policy-svc: emergency suspension of a platform-wide policy version'),
    ('POLICY_VERSION_RETIRE',          'LEGACY', 'STANDARD', 'Enforced by policy-svc: retire a policy version'),
    ('POLICY_VERSION_RETIRE_GLOBAL',   'LEGACY', 'HIGH',     'Enforced by policy-svc: retire a platform-wide policy version')
ON CONFLICT (action_name) DO NOTHING;
