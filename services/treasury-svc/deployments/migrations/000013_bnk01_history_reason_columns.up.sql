-- SPEC_DEVIATIONS A6: suspend_reason and close_reason are stored on
-- bank_accounts (migration 000003) but migration 000006's own history
-- snapshot — whose doc comment promises "every field on bank_accounts that
-- a BNK-01 command can change" — omitted both, even though Suspend and
-- Close are exactly such commands. The §7 evidence line asks for "prior/new
-- values, source, verifier, effective dates, … actor/approver and reason".

ALTER TABLE bank_account_history
    ADD COLUMN suspend_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN close_reason TEXT NOT NULL DEFAULT '';

-- 000006's reject_account_history_mutation enumerates every append-only
-- column explicitly; the two new ones must join that list, or they would be
-- the one way left to silently rewrite a standing history row.
CREATE OR REPLACE FUNCTION reject_account_history_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_account_history rows are never deleted';
    END IF;
    IF NEW.bank_account_id IS DISTINCT FROM OLD.bank_account_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.account_name IS DISTINCT FROM OLD.account_name
        OR NEW.masked_account_number IS DISTINCT FROM OLD.masked_account_number
        OR NEW.bank_identifier IS DISTINCT FROM OLD.bank_identifier
        OR NEW.account_status IS DISTINCT FROM OLD.account_status
        OR NEW.branch_ref IS DISTINCT FROM OLD.branch_ref
        OR NEW.country IS DISTINCT FROM OLD.country
        OR NEW.account_type IS DISTINCT FROM OLD.account_type
        OR NEW.requested_operational_use IS DISTINCT FROM OLD.requested_operational_use
        OR NEW.token_version IS DISTINCT FROM OLD.token_version
        OR NEW.changed_by_principal_id IS DISTINCT FROM OLD.changed_by_principal_id
        OR NEW.effective_at IS DISTINCT FROM OLD.effective_at
        OR NEW.suspend_reason IS DISTINCT FROM OLD.suspend_reason
        OR NEW.close_reason IS DISTINCT FROM OLD.close_reason
    THEN
        RAISE EXCEPTION 'bank account history row % is append-only — only superseded_by may ever be set', OLD.history_id;
    END IF;
    IF OLD.superseded_by IS NOT NULL AND NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'bank account history row % is already superseded', OLD.history_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
