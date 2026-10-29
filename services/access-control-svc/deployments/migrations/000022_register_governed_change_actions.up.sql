-- Migration 000022: register the approval and publication actions
-- authorization-svc's governed changes check (its 000034-000036; auth-service
-- audit AU-02, AU-03, AU-17, 22 Oct 2026). A bundle may only name registered
-- actions, so until these exist the checker side of each maker-checker can
-- only answer DENIED.
--
--   iam.break_glass.approve      GOV-12 ApproveBreakGlass: an independent,
--                                stepped-up approver activates a break-glass
--                                request.
--   iam.authority_limit.manage   request a new authority limit, a change, or a
--                                retirement (ZS-IAM-001 §12, §21).
--   iam.authority_limit.approve  decide one; never the requester or the holder.
--   iam.sod_rule.publish         publish a requested SoD rule change (Appendix A
--                                iam.sod_rule.read/publish); also decides SoD
--                                exceptions.
--   iam.policy.publish           publish a requested ABAC rule change (Appendix
--                                A iam.policy.read/publish).
--
-- All are protected platform-admin and CRITICAL, like the other iam.*
-- administration powers: a tenant cannot put them in its own roles, and an
-- assignment of one needs independent approval.

INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('iam.break_glass.approve',     'Approve break-glass requests', 'platform_admin'),
    ('iam.authority_limit.manage',  'Request authority limit changes', 'platform_admin'),
    ('iam.authority_limit.approve', 'Approve authority limit changes', 'platform_admin'),
    ('iam.sod_rule.publish',        'Publish SoD rule changes and decide SoD exceptions', 'platform_admin'),
    ('iam.policy.publish',          'Publish ABAC rule changes', 'platform_admin')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('iam.break_glass.approve',     'TAXONOMY', 'CRITICAL', 'Enforced by authorization-svc: approve a break-glass request (independent, stepped-up)'),
    ('iam.authority_limit.manage',  'TAXONOMY', 'CRITICAL', 'Enforced by authorization-svc: request a new, changed or retired approval / signing authority limit'),
    ('iam.authority_limit.approve', 'TAXONOMY', 'CRITICAL', 'Enforced by authorization-svc: approve or reject an authority limit change'),
    ('iam.sod_rule.publish',        'TAXONOMY', 'CRITICAL', 'Enforced by authorization-svc: publish a SoD rule change; decide SoD exceptions'),
    ('iam.policy.publish',          'TAXONOMY', 'CRITICAL', 'Enforced by authorization-svc: publish an ABAC rule change')
ON CONFLICT (action_name) DO NOTHING;
