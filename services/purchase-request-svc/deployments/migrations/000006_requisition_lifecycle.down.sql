DROP TRIGGER IF EXISTS trg_guard_purchase_request_line ON purchase_request_lines;
DROP TRIGGER IF EXISTS trg_guard_purchase_request ON purchase_requests;
DROP TRIGGER IF EXISTS trg_pr_history_append_only ON purchase_request_history;
DROP FUNCTION IF EXISTS guard_purchase_request_line();
DROP FUNCTION IF EXISTS guard_purchase_request();
DROP FUNCTION IF EXISTS reject_pr_history_mutation();
DROP TABLE IF EXISTS purchase_request_history;
DROP TABLE IF EXISTS purchase_request_lines;
DROP INDEX IF EXISTS uq_purchase_requests_converted_po;
ALTER TABLE purchase_requests
    DROP CONSTRAINT IF EXISTS purchase_requests_converted_has_po,
    DROP CONSTRAINT IF EXISTS purchase_requests_budget_decision_known,
    DROP CONSTRAINT IF EXISTS purchase_requests_status_known;
-- Collapse the richer lifecycle back to the original three states.
UPDATE purchase_requests SET status = 'PENDING' WHERE status IN ('DRAFT', 'PENDING_APPROVAL');
UPDATE purchase_requests SET status = 'APPROVED' WHERE status = 'CONVERTED';
UPDATE purchase_requests SET status = 'REJECTED' WHERE status IN ('CANCELLED', 'EXPIRED');
ALTER TABLE purchase_requests
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS business_purpose,
    DROP COLUMN IF EXISTS cost_center,
    DROP COLUMN IF EXISTS project_ref,
    DROP COLUMN IF EXISTS budget_ref,
    DROP COLUMN IF EXISTS preferred_supplier_ref,
    DROP COLUMN IF EXISTS required_date,
    DROP COLUMN IF EXISTS attachment_refs,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS submitted_by_principal_id,
    DROP COLUMN IF EXISTS submitted_at,
    DROP COLUMN IF EXISTS last_amended_by_principal_id,
    DROP COLUMN IF EXISTS budget_decision,
    DROP COLUMN IF EXISTS budget_basis,
    DROP COLUMN IF EXISTS approval_invalidated_count,
    DROP COLUMN IF EXISTS cancelled_by_principal_id,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS cancellation_reason,
    DROP COLUMN IF EXISTS converted_purchase_order_id,
    DROP COLUMN IF EXISTS converted_by_principal_id,
    DROP COLUMN IF EXISTS converted_at,
    DROP COLUMN IF EXISTS updated_at;
