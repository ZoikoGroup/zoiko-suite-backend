DROP TABLE IF EXISTS inventory_write_downs;
ALTER TABLE inventory_valuation_entries DROP CONSTRAINT IF EXISTS fk_valuation_entry_run;
DROP TABLE IF EXISTS inventory_valuation_runs;
DROP TABLE IF EXISTS inventory_layer_consumptions;
DROP TABLE IF EXISTS inventory_valuation_entries;
DROP TABLE IF EXISTS inventory_cost_layers;
