-- Migration 000020: register the auth-plane actions that services check but
-- no role could grant (auth plane closure, 14 Oct 2026).
--
-- A bundle may only name registered actions, so until these exist every gate
-- that asks for them can only answer DENIED:
--
--   IDENTITY_MFA_RESET   identity-context-svc POST /v1/principals/{id}/mfa/reset
--                        (an administrator retires a lost second factor).
--                        HIGH: it is credential recovery, "separately
--                        controlled" (ZS-IAM-001 §3), so an assignment needs
--                        independent approval.
--   iam.workload.manage  authorization-svc /v1/admin/workload-bindings
--                        (Governance audit N-09). CRITICAL like the other
--                        iam.* administration powers.
--   MTLS_*               mtls-management-svc's certificate and policy gates
--                        (Security & Trust §14). Issuing, revoking and policy
--                        writes are HIGH; reads and rotation STANDARD.

INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('iam.workload.manage', 'Bind workload identities to the actions they may perform', 'platform_admin')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('IDENTITY_MFA_RESET',   'LEGACY',   'HIGH',     'Enforced by identity-context-svc: reset another principal''s second factor (credential recovery)'),
    ('iam.workload.manage',  'TAXONOMY', 'CRITICAL', 'Enforced by authorization-svc: create, deactivate and reactivate workload bindings'),
    ('MTLS_CERT_PROVISION',  'LEGACY',   'HIGH',     'Enforced by mtls-management-svc: issue a service certificate'),
    ('MTLS_CERT_READ',       'LEGACY',   'STANDARD', 'Enforced by mtls-management-svc: read certificate inventory'),
    ('MTLS_CERT_ROTATE',     'LEGACY',   'STANDARD', 'Enforced by mtls-management-svc: rotate a service certificate'),
    ('MTLS_CERT_REVOKE',     'LEGACY',   'HIGH',     'Enforced by mtls-management-svc: revoke a service certificate'),
    ('MTLS_POLICY_READ',     'LEGACY',   'STANDARD', 'Enforced by mtls-management-svc: read mTLS policy'),
    ('MTLS_POLICY_WRITE',    'LEGACY',   'HIGH',     'Enforced by mtls-management-svc: change mTLS policy')
ON CONFLICT (action_name) DO NOTHING;
