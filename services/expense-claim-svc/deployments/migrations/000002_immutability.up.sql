-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

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

-- Expense lines may be amended only while their parent claim is still
-- editable (DRAFT or RETURNED) — this also covers the SubmitExpenseClaim
-- flow, which writes each line's real tax-determination result BEFORE
-- flipping the parent claim's status to PENDING_APPROVAL in the same
-- transaction, so that write is still permitted at the moment it happens.
-- Never deletable, at any status — no row here is ever removed.
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

CREATE TRIGGER trg_reject_expense_line_mutation
    BEFORE INSERT OR UPDATE OR DELETE ON expense_lines
    FOR EACH ROW EXECUTE FUNCTION reject_expense_line_mutation();

-- Pure append-only guard for the audit trail.
CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_expense_claim_events_mutation
    BEFORE UPDATE OR DELETE ON expense_claim_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
