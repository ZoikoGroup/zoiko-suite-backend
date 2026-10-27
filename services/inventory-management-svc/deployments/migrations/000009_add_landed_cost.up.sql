-- Migration: 000009_add_landed_cost.up.sql
--
-- INV-04 AllocateLandedCost. Spec §10 "Late-arriving cost": "Late freight/
-- landed cost or invoice correction creates controlled revaluation/
-- reallocation and accounting correction according period state; closed-
-- period history is not silently rewritten." Negative path: "Late landed cost
-- silently rewrites closed-period COGS."
--
-- Model. A landed cost is applied to the cost layer of one valued RECEIPT:
--   * the part belonging to the units STILL ON HAND (inventory_share) raises
--     that layer's unit_cost by unit_uplift = inventory_share / remaining;
--   * the part belonging to units ALREADY CONSUMED (cogs_share) is a true-up
--     of cost of goods sold — those units were issued at the old cost.
--   * one balanced journal: Dr Inventory (inventory_share), Dr COGS
--     (cogs_share), Cr offset account (amount), posted in the CURRENT open
--     fiscal_period — a forward correction; the period the receipt originally
--     fell in is never reopened or rewritten.
-- Historic valuation entries and layer consumptions are untouched (they stay
-- the as-it-was record); GetInventoryValueAsOf reconstructs history by
-- removing the uplift of allocations created after the as-of instant.
--
-- This row is append-only evidence: economic fields can never change after
-- insert (trigger); only the posting status/journal link may advance.
-- PENDING_POSTING -> ACCOUNTING_EVENT_EMITTED lets a posting that failed
-- after the layer was updated be retried idempotently (same idempotency_key).
CREATE TABLE inventory_landed_cost_allocations (
    allocation_id                  UUID PRIMARY KEY,
    tenant_id                        VARCHAR(255) NOT NULL,
    legal_entity_id                    VARCHAR(255) NOT NULL,
    item_id                              UUID NOT NULL REFERENCES inventory_items(item_id),
    location_id                            UUID NOT NULL REFERENCES inventory_locations(location_id),
    movement_id                              UUID NOT NULL REFERENCES inventory_movements(movement_id),
    layer_id                                   UUID NOT NULL REFERENCES inventory_cost_layers(layer_id),
    idempotency_key                              VARCHAR(255) NOT NULL,
    amount                                         NUMERIC(18,2) NOT NULL,
    inventory_share                                  NUMERIC(18,2) NOT NULL,
    cogs_share                                         NUMERIC(18,2) NOT NULL,
    remaining_quantity_at_allocation                     NUMERIC(18,4) NOT NULL,
    unit_uplift                                            NUMERIC(18,6) NOT NULL,
    valuation_evidence_ref                                   VARCHAR(255) NOT NULL,
    fiscal_period                                              VARCHAR(20) NOT NULL,
    inventory_account_code                                       VARCHAR(64) NOT NULL,
    cogs_account_code                                              VARCHAR(64) NOT NULL,
    offset_account_code                                              VARCHAR(64) NOT NULL,
    status                                                             VARCHAR(30) NOT NULL,
    journal_id                                                           VARCHAR(255),
    created_at                                                             TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                                                  VARCHAR(255) NOT NULL,
    emitted_at                                                                 TIMESTAMP WITH TIME ZONE,

    CONSTRAINT chk_landed_cost_amount_positive CHECK (amount > 0),
    CONSTRAINT chk_landed_cost_shares_sum CHECK (inventory_share >= 0 AND cogs_share >= 0 AND inventory_share + cogs_share = amount),
    CONSTRAINT chk_landed_cost_status CHECK (status IN ('PENDING_POSTING', 'ACCOUNTING_EVENT_EMITTED')),
    UNIQUE (tenant_id, idempotency_key)
);

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

CREATE TRIGGER trg_reject_landed_cost_mutation
    BEFORE UPDATE OR DELETE ON inventory_landed_cost_allocations
    FOR EACH ROW EXECUTE FUNCTION reject_landed_cost_economic_mutation();

ALTER TABLE inventory_landed_cost_allocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_landed_cost_allocations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_landed_cost_allocations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_landed_cost_layer ON inventory_landed_cost_allocations (tenant_id, layer_id);
CREATE INDEX idx_landed_cost_movement ON inventory_landed_cost_allocations (tenant_id, movement_id);
