-- Restore migration 004's terminal-statement rule exactly. Down migrations
-- are destructive by definition; this one only removes the narrow reprocess
-- exception introduced by 008.
CREATE OR REPLACE FUNCTION reject_terminal_statement_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_statements rows are never deleted';
    END IF;
    IF OLD.status NOT IN ('RECEIVED', 'VALIDATING') THEN
        RAISE EXCEPTION 'bank statement % is % and can no longer be modified', OLD.statement_id, OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
