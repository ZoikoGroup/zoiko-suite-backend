-- Migration 000013 down. Rows already EXPIRED cannot satisfy the old status
-- CHECK; they are recorded as REVOKED (the assignment did end) before it is
-- restored.
ALTER TABLE access_review_items DROP COLUMN IF EXISTS subject_status;
ALTER TABLE access_review_items DROP COLUMN IF EXISTS last_granted_at;
ALTER TABLE access_review_campaigns DROP COLUMN IF EXISTS dormancy_days;

DELETE FROM protected_permissions WHERE action_name = 'iam.assignment.approve_privileged';

DROP POLICY IF EXISTS assignment_requests_expiry_scan ON assignment_requests;
DROP INDEX IF EXISTS idx_assignment_requests_due;

UPDATE assignment_requests SET status = 'REVOKED' WHERE status = 'EXPIRED';
ALTER TABLE assignment_requests DROP CONSTRAINT IF EXISTS assignment_requests_end_after_start;
ALTER TABLE assignment_requests DROP CONSTRAINT IF EXISTS assignment_requests_provisioned_has_assignment;
ALTER TABLE assignment_requests ADD CONSTRAINT assignment_requests_provisioned_has_assignment CHECK (
    status NOT IN ('PROVISIONED', 'REVOKED') OR authz_assignment_id IS NOT NULL
);
ALTER TABLE assignment_requests DROP CONSTRAINT IF EXISTS assignment_requests_status_check;
ALTER TABLE assignment_requests ADD CONSTRAINT assignment_requests_status_check CHECK (status IN
    ('PENDING_APPROVAL', 'PROVISIONED', 'REJECTED', 'CANCELLED', 'REVOKED'));
ALTER TABLE assignment_requests DROP COLUMN IF EXISTS effective_to;
-- The template version and permission definition stay: role_template_versions
-- is append-only by design, and a published template outlives the release
-- that published it.
