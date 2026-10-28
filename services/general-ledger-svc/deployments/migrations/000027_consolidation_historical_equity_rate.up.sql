-- Global Accounting Kernel follow-up: historical-rate translation of a member's equity (IAS 21.39(c): equity items are not
-- retranslated at each closing rate). A member that carries capital terms may also carry the date its equity arose; the equity
-- accounts it names are then translated at the spot rate of that date, and the difference to the closing rate falls into the
-- translation reserve with every other rate difference.

ALTER TABLE consolidation_members
    ADD COLUMN equity_rate_date DATE,
    ADD CONSTRAINT consolidation_members_equity_rate CHECK (
        equity_rate_date IS NULL OR (equity_accounts <> '[]'::jsonb AND equity_rate_date <= effective_from));

-- the date is part of the immutable terms of a membership: it can no more be changed than the accounts it applies to
CREATE OR REPLACE FUNCTION consolidation_member_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation membership is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL
       OR (NEW.member_id, NEW.tenant_id, NEW.group_id, NEW.legal_entity_id, NEW.source_book_id, NEW.method, NEW.ownership_pct, NEW.effective_from, NEW.created_by, NEW.created_at,
           NEW.investment_account, NEW.investor_entity_id, NEW.equity_accounts::text, NEW.equity_rate_date)
          IS DISTINCT FROM (OLD.member_id, OLD.tenant_id, OLD.group_id, OLD.legal_entity_id, OLD.source_book_id, OLD.method, OLD.ownership_pct, OLD.effective_from, OLD.created_by, OLD.created_at,
           OLD.investment_account, OLD.investor_entity_id, OLD.equity_accounts::text, OLD.equity_rate_date) THEN
        RAISE EXCEPTION 'a consolidation membership can only be ended, once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
