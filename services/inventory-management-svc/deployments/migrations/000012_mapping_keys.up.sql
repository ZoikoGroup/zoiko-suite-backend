-- Migration: 000012_mapping_keys.up.sql
--
-- ACC-02: the general ledger resolves posting accounts from current,
-- versioned account mappings. A subledger may name a governed mapping key,
-- never a raw GL account code. These columns persist the mapping key that
-- was used when a run/write-down/allocation was created, so a later
-- mapping version change cannot silently reinterpret historical postings.
--
-- The legacy *_account_code columns stay in place for rows created before
-- this migration. New rows leave them empty and use the mapping-key columns
-- below; the application never posts from the legacy columns.

ALTER TABLE inventory_valuation_runs
    ADD COLUMN inventory_mapping_key VARCHAR(128),
    ADD COLUMN cogs_mapping_key      VARCHAR(128);

ALTER TABLE inventory_write_downs
    ADD COLUMN expense_mapping_key   VARCHAR(128),
    ADD COLUMN inventory_mapping_key VARCHAR(128);

ALTER TABLE inventory_landed_cost_allocations
    ADD COLUMN inventory_mapping_key VARCHAR(128),
    ADD COLUMN cogs_mapping_key      VARCHAR(128),
    ADD COLUMN offset_mapping_key    VARCHAR(128);

-- The landed-cost row is append-only evidence (migration 000009); the
-- mapping keys used to post it are as economic as the account codes they
-- replace, so they join the same immutability guard.
CREATE OR REPLACE FUNCTION reject_landed_cost_economic_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'inventory_landed_cost_allocations: DELETE is not permitted';
    END IF;
    IF NEW.allocation_id IS DISTINCT FROM OLD.allocation_id OR
       NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR
       NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR
       NEW.item_id IS DISTINCT FROM OLD.item_id OR
       NEW.location_id IS DISTINCT FROM OLD.location_id OR
       NEW.movement_id IS DISTINCT FROM OLD.movement_id OR
       NEW.layer_id IS DISTINCT FROM OLD.layer_id OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR
       NEW.amount IS DISTINCT FROM OLD.amount OR
       NEW.inventory_share IS DISTINCT FROM OLD.inventory_share OR
       NEW.cogs_share IS DISTINCT FROM OLD.cogs_share OR
       NEW.remaining_quantity_at_allocation IS DISTINCT FROM OLD.remaining_quantity_at_allocation OR
       NEW.unit_uplift IS DISTINCT FROM OLD.unit_uplift OR
       NEW.valuation_evidence_ref IS DISTINCT FROM OLD.valuation_evidence_ref OR
       NEW.fiscal_period IS DISTINCT FROM OLD.fiscal_period OR
       NEW.inventory_account_code IS DISTINCT FROM OLD.inventory_account_code OR
       NEW.cogs_account_code IS DISTINCT FROM OLD.cogs_account_code OR
       NEW.offset_account_code IS DISTINCT FROM OLD.offset_account_code OR
       NEW.inventory_mapping_key IS DISTINCT FROM OLD.inventory_mapping_key OR
       NEW.cogs_mapping_key IS DISTINCT FROM OLD.cogs_mapping_key OR
       NEW.offset_mapping_key IS DISTINCT FROM OLD.offset_mapping_key OR
       NEW.created_at IS DISTINCT FROM OLD.created_at OR
       NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'inventory_landed_cost_allocations: economic fields are immutable — a correction is a new allocation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
