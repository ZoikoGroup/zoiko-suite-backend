-- Migration 000013: assignment end dates, effective-dated revocation, tiered
-- approval authority and the review signals §24 asks for (7 Oct 2026 re-audit).
--
-- 1. assignment_requests.effective_to. §9 and §22 define an access assignment
--    as "subject/group + role/policy + scope + effective DATES"; this table had
--    only effective_from, and §9's revocation is "immediate OR effective-dated
--    removal", where only immediate existed. effective_to is set at grant time
--    or by an effective-dated revoke, and is provisioned into authorization-svc
--    (which enforces it at the instant). A PROVISIONED row whose effective_to
--    has passed is closed by the expiry sweep: EXPIRED when the end was set at
--    grant, REVOKED when someone scheduled it; either way iam.assignment.revoked
--    ("assignment revoked/expired", §23) is enqueued so sessions end.
ALTER TABLE assignment_requests ADD COLUMN IF NOT EXISTS effective_to TIMESTAMPTZ;

ALTER TABLE assignment_requests DROP CONSTRAINT IF EXISTS assignment_requests_status_check;
ALTER TABLE assignment_requests ADD CONSTRAINT assignment_requests_status_check CHECK (status IN
    ('PENDING_APPROVAL', 'PROVISIONED', 'REJECTED', 'CANCELLED', 'REVOKED', 'EXPIRED'));

ALTER TABLE assignment_requests DROP CONSTRAINT IF EXISTS assignment_requests_provisioned_has_assignment;
ALTER TABLE assignment_requests ADD CONSTRAINT assignment_requests_provisioned_has_assignment CHECK (
    status NOT IN ('PROVISIONED', 'REVOKED', 'EXPIRED') OR authz_assignment_id IS NOT NULL
);

ALTER TABLE assignment_requests DROP CONSTRAINT IF EXISTS assignment_requests_end_after_start;
ALTER TABLE assignment_requests ADD CONSTRAINT assignment_requests_end_after_start CHECK (
    effective_to IS NULL OR effective_to > effective_from
);

-- The sweep's claim query: provisioned rows with an end, soonest first.
CREATE INDEX IF NOT EXISTS idx_assignment_requests_due
    ON assignment_requests (effective_to) WHERE status = 'PROVISIONED' AND effective_to IS NOT NULL;

-- The sweep crosses tenants to FIND due rows, then closes each one in a
-- transaction scoped to that row's tenant (so the event it enqueues passes the
-- outbox's tenant policy like any request's). It names itself through
-- app.assignment_expiry, and is admitted for SELECT only: it cannot write any
-- tenant's rows through this disjunct.
DROP POLICY IF EXISTS assignment_requests_expiry_scan ON assignment_requests;
CREATE POLICY assignment_requests_expiry_scan ON assignment_requests FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.assignment_expiry', true), ''), 'false') = 'true');

-- 2. Approval authority by risk (§9: "may require manager/data-owner/security
--    approval depending on risk"). Any independent ROLE_MANAGE holder approved
--    every tier, CRITICAL included, so the approver of a protected grant needed
--    no more authority than its requester. A CRITICAL request now needs an
--    approver who also holds iam.assignment.approve_privileged. Protected, so
--    no tenant custom role can include it; it comes from the
--    SECURITY_ACCESS_APPROVER template below or from platform provisioning.
INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('iam.assignment.approve_privileged', 'TAXONOMY', 'CRITICAL', 'Approve a CRITICAL-risk access assignment request (security approval)')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('iam.assignment.approve_privileged', 'Approve CRITICAL-risk access assignments', 'platform_admin')
ON CONFLICT (action_name) DO UPDATE SET active_flag = TRUE, updated_at = now();

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('SECURITY_ACCESS_APPROVER', 'Security Access Approver',
     'Security approval of CRITICAL-risk access assignment requests; provisions what it approves.', 'TENANT', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('SECURITY_ACCESS_APPROVER', 1, ARRAY['iam.assignment.approve_privileged','iam.assignment.grant','iam.assignment.list'],
     'Security approver for CRITICAL assignment requests (7 Oct 2026 re-audit)', 'migration:000013')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- 3. Review signals (§24 "Dormancy", "Orphan detection"). A campaign records
--    the dormancy window it applied; an item records what it was flagged on.
ALTER TABLE access_review_campaigns ADD COLUMN IF NOT EXISTS dormancy_days INTEGER NOT NULL DEFAULT 90
    CHECK (dormancy_days BETWEEN 1 AND 3650);
ALTER TABLE access_review_items ADD COLUMN IF NOT EXISTS last_granted_at TIMESTAMPTZ;
ALTER TABLE access_review_items ADD COLUMN IF NOT EXISTS subject_status VARCHAR(32);
