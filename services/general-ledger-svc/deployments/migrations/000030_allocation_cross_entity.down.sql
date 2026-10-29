CREATE OR REPLACE FUNCTION allocation_runs_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an allocation run is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.allocation_run_id, NEW.tenant_id, NEW.allocation_rule_id, NEW.legal_entity_id, NEW.accounting_book_id, NEW.fiscal_period, NEW.ledger_watermark, NEW.pool_amount,
        NEW.population::text, NEW.journal_id, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.allocation_run_id, OLD.tenant_id, OLD.allocation_rule_id, OLD.legal_entity_id, OLD.accounting_book_id, OLD.fiscal_period, OLD.ledger_watermark, OLD.pool_amount,
        OLD.population::text, OLD.journal_id, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'what an allocation run took and posted is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'REVERSED' THEN
        RAISE EXCEPTION 'a reversed allocation run is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE allocation_runs DROP COLUMN IF EXISTS remote_reversals, DROP COLUMN IF EXISTS remote_journals;
