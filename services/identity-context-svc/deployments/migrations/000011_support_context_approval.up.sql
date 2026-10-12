-- Migration 000011: break-glass approval is the approver's own act
-- (Group 1 re-audit gap S1-1 / R-2; GOV-01 §4 AttachSupportContext, §1
-- "independently approved", §2 invariant 8).
--
-- approver_principal_id used to be a request-body field: the requester NAMED
-- an approver, who never consented and need not exist, and the grant was live
-- at once. Now an attach records a PENDING_APPROVAL request that grants
-- nothing; the named approver approves it with their own call, and only then
-- do granted_at / expires_at start and the attached event go out.
--
-- Rows from before this migration were granted under the old rule and stay
-- APPROVED (the default); requested_by_principal_id is unknown for them.

ALTER TABLE support_contexts
    ADD COLUMN IF NOT EXISTS requested_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS approval_status VARCHAR(20) NOT NULL DEFAULT 'APPROVED',
    ADD COLUMN IF NOT EXISTS approved_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS requested_ttl_seconds INTEGER;

ALTER TABLE support_contexts DROP CONSTRAINT IF EXISTS support_contexts_approval_status_check;
ALTER TABLE support_contexts ADD CONSTRAINT support_contexts_approval_status_check
    CHECK (approval_status IN ('PENDING_APPROVAL', 'APPROVED'));

-- The requester never approves their own request (maker-checker, GOV-12).
ALTER TABLE support_contexts DROP CONSTRAINT IF EXISTS support_contexts_requester_not_approver_check;
ALTER TABLE support_contexts ADD CONSTRAINT support_contexts_requester_not_approver_check
    CHECK (requested_by_principal_id IS NULL OR requested_by_principal_id <> approver_principal_id);

-- A request made under this rule is approved only with a recorded approval.
ALTER TABLE support_contexts DROP CONSTRAINT IF EXISTS support_contexts_approved_has_time_check;
ALTER TABLE support_contexts ADD CONSTRAINT support_contexts_approved_has_time_check
    CHECK (approval_status <> 'APPROVED' OR requested_by_principal_id IS NULL OR approved_at IS NOT NULL);
