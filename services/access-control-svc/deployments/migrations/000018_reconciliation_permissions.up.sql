-- Migration 000018: register reconciliation-engine-svc's non-basic actions
-- (SPEC_DEVIATIONS A9/A11/A12, B34). RECONCILIATION_MANAGE and
-- RECONCILIATION_READ were registered in 000008; CERTIFY, ACCEPT_RISK,
-- APPROVE_RULES and REVOKE never were. Without this row no role can hold
-- any of them, so certifying, accepting risk, approving a rule version and
-- revoking a certification have all been refused (403) in any environment
-- with a real access-control-svc, independent of today's REVOKE addition.
--
-- LEGACY naming and STANDARD risk tier, matching RECONCILIATION_MANAGE/
-- READ's own entries in 000008.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('RECONCILIATION_CERTIFY', 'LEGACY', 'STANDARD', ''),
    ('RECONCILIATION_ACCEPT_RISK', 'LEGACY', 'STANDARD', ''),
    ('RECONCILIATION_APPROVE_RULES', 'LEGACY', 'STANDARD', ''),
    ('RECONCILIATION_REVOKE', 'LEGACY', 'STANDARD', '')
ON CONFLICT (action_name) DO NOTHING;
