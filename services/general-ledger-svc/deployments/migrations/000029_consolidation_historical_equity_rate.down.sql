CREATE OR REPLACE FUNCTION consolidation_member_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation membership is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL
       OR (NEW.member_id, NEW.tenant_id, NEW.group_id, NEW.legal_entity_id, NEW.source_book_id, NEW.method, NEW.ownership_pct, NEW.effective_from, NEW.created_by, NEW.created_at,
           NEW.investment_account, NEW.investor_entity_id, NEW.equity_accounts::text)
          IS DISTINCT FROM (OLD.member_id, OLD.tenant_id, OLD.group_id, OLD.legal_entity_id, OLD.source_book_id, OLD.method, OLD.ownership_pct, OLD.effective_from, OLD.created_by, OLD.created_at,
           OLD.investment_account, OLD.investor_entity_id, OLD.equity_accounts::text) THEN
        RAISE EXCEPTION 'a consolidation membership can only be ended, once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE consolidation_members
    DROP CONSTRAINT IF EXISTS consolidation_members_equity_rate,
    DROP COLUMN IF EXISTS equity_rate_date;
