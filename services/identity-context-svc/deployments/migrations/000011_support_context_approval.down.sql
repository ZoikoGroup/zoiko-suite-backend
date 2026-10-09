ALTER TABLE support_contexts DROP CONSTRAINT IF EXISTS support_contexts_approved_has_time_check;
ALTER TABLE support_contexts DROP CONSTRAINT IF EXISTS support_contexts_requester_not_approver_check;
ALTER TABLE support_contexts DROP CONSTRAINT IF EXISTS support_contexts_approval_status_check;
ALTER TABLE support_contexts
    DROP COLUMN IF EXISTS requested_ttl_seconds,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approval_status,
    DROP COLUMN IF EXISTS requested_by_principal_id;
