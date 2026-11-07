-- Migration 000043: register the ORG-01 merge action (ZS-SVC-I-001 s4.1), enforced by identity-context-svc.
-- Merging decides which of two records is the person: the duplicate is disabled for good, its sessions end and its
-- provider links and role assignments move. CRITICAL. identity-context-svc refuses a merge by either identity
-- itself, whoever holds the permission. Who holds it is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('IDENTITY_MERGE', 'LEGACY', 'CRITICAL', 'Enforced by identity-context-svc: merge a duplicate identity into a surviving one; never by either identity')
ON CONFLICT (action_name) DO NOTHING;
