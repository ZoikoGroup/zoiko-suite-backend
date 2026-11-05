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
       NEW.created_at IS DISTINCT FROM OLD.created_at OR
       NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'inventory_landed_cost_allocations: economic fields are immutable — a correction is a new allocation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE inventory_landed_cost_allocations
    DROP COLUMN IF EXISTS offset_mapping_key,
    DROP COLUMN IF EXISTS cogs_mapping_key,
    DROP COLUMN IF EXISTS inventory_mapping_key;

ALTER TABLE inventory_write_downs
    DROP COLUMN IF EXISTS inventory_mapping_key,
    DROP COLUMN IF EXISTS expense_mapping_key;

ALTER TABLE inventory_valuation_runs
    DROP COLUMN IF EXISTS cogs_mapping_key,
    DROP COLUMN IF EXISTS inventory_mapping_key;
