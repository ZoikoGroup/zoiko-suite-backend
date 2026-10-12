-- Migration: 000010_add_layer_rebuilds.up.sql
--
-- INV-04 RebuildCostLayersControlled. Spec §10 "Inventory Valuation Method
-- Governance": historic evidence is never rewritten. A rebuild only restores
-- a cost layer's remaining_quantity to what its immutable evidence implies
-- (original_quantity minus the sum of recorded consumptions); unit_cost,
-- original_quantity, consumptions and valuation entries are never touched.
--
-- This table is the append-only audit of every such correction: who, why,
-- and the before/after quantity. Rows are written in the same transaction as
-- the layer change. UPDATE and DELETE are rejected by trigger.
CREATE TABLE inventory_layer_rebuilds (
    rebuild_id                   UUID PRIMARY KEY,
    tenant_id                      VARCHAR(255) NOT NULL,
    layer_id                         UUID NOT NULL REFERENCES inventory_cost_layers(layer_id),
    item_id                            UUID NOT NULL REFERENCES inventory_items(item_id),
    location_id                          UUID NOT NULL REFERENCES inventory_locations(location_id),
    old_remaining                          NUMERIC(18,4) NOT NULL,
    new_remaining                            NUMERIC(18,4) NOT NULL,
    reason                                     TEXT NOT NULL,
    rebuilt_by_principal_id                      VARCHAR(255) NOT NULL,
    created_at                                     TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT chk_layer_rebuild_new_nonnegative CHECK (new_remaining >= 0),
    CONSTRAINT chk_layer_rebuild_reason CHECK (length(btrim(reason)) > 0)
);

CREATE OR REPLACE FUNCTION reject_layer_rebuild_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'inventory_layer_rebuilds is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_layer_rebuild_mutation
    BEFORE UPDATE OR DELETE ON inventory_layer_rebuilds
    FOR EACH ROW EXECUTE FUNCTION reject_layer_rebuild_mutation();

ALTER TABLE inventory_layer_rebuilds ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_layer_rebuilds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_layer_rebuilds
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_layer_rebuilds_layer ON inventory_layer_rebuilds (tenant_id, layer_id);
