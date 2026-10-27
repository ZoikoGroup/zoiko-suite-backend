DROP TABLE IF EXISTS expense_claim_submissions;

-- Restore the 000002 triggers.
CREATE OR REPLACE FUNCTION reject_expense_line_mutation() RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
    check_claim_id UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'expense_lines rows are never deleted';
    END IF;
    check_claim_id := NEW.claim_id;
    SELECT status INTO parent_status FROM expense_claims WHERE claim_id = check_claim_id;
    IF parent_status NOT IN ('DRAFT', 'RETURNED') THEN
        RAISE EXCEPTION 'claim % is in status % and can no longer accept new or amended expense lines', check_claim_id, parent_status
            USING ERRCODE = 'ZK001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Drop the new-state guard before narrowing the CHECK; rows in a state the
-- old schema cannot represent are folded back to the nearest old state.
DROP TRIGGER IF EXISTS trg_reject_expense_claim_mutation ON expense_claims;
UPDATE expense_claims SET status = 'REIMBURSABLE' WHERE status = 'CLOSED';
UPDATE expense_claims SET status = 'PENDING_APPROVAL' WHERE status = 'SUBMITTED';

CREATE OR REPLACE FUNCTION reject_expense_claim_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'expense_claims rows are never deleted';
    END IF;
    IF OLD.status IN ('REJECTED', 'REIMBURSABLE', 'CANCELLED') THEN
        RAISE EXCEPTION 'expense claim % is in terminal status % and cannot be modified', OLD.claim_id, OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_expense_claim_mutation
    BEFORE UPDATE OR DELETE ON expense_claims
    FOR EACH ROW EXECUTE FUNCTION reject_expense_claim_mutation();

DROP FUNCTION IF EXISTS expense_claim_transition_allowed(TEXT, TEXT);

DROP INDEX IF EXISTS uq_expense_lines_receipt_document;
CREATE UNIQUE INDEX uq_expense_lines_receipt_document
    ON expense_lines (receipt_document_id)
    WHERE receipt_document_id IS NOT NULL;

ALTER TABLE expense_lines DROP COLUMN voided_at, DROP COLUMN void_reason;

ALTER TABLE expense_claims DROP CONSTRAINT expense_claims_status_check;
ALTER TABLE expense_claims ADD CONSTRAINT expense_claims_status_check
    CHECK (status IN ('DRAFT', 'PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'RETURNED', 'REIMBURSABLE', 'CANCELLED'));

ALTER TABLE expense_claims
    DROP COLUMN version, DROP COLUMN submitted_version, DROP COLUMN payable_id, DROP COLUMN payable_state,
    DROP COLUMN payable_blocked_reason, DROP COLUMN payee_destination_id, DROP COLUMN closed_at, DROP COLUMN close_reason;
