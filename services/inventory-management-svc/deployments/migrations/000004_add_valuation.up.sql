-- Migration: 000004_add_valuation.up.sql
--
-- INV-04 (Inventory Valuation): "owns InventoryCostLayer/Pool; ValuationEntry;
-- unit cost; inventory value; consumed cost; landed/conversion cost
-- allocation; write-down/reversal; valuation run/version. Must never own:
-- Physical quantity truth or commercial pricing." Purpose (verbatim):
-- "Assign deterministic monetary cost to inventory movements and on-hand
-- stock using effective-dated accounting-policy methods and source-linked
-- cost evidence."
--
-- State model (verbatim): "Movement valuation Draft→Calculated→Validated→
-- Final; run PopulationFrozen→Calculated→Approved→AccountingEventEmitted→
-- Reconciled; adjustments supersede prior state." The same doc-vs-command
-- mismatch pattern recurring throughout this build hits INV-04 too:
-- ValueMovement is the ONLY command that touches a movement's own
-- valuation, and it lands the result directly in FINAL — no command
-- reaches Draft/Calculated/Validated independently, the same collapse
-- AST-02's own BuildDepreciationSchedule already used. On the run side,
-- CreateValuationRun (Draft), an implicit freeze (PopulationFrozen —
-- folded into CreateValuationRun itself, since "population" here is just
-- "every FINAL valuation entry in this period not yet claimed by a run",
-- computed at creation time rather than via a separate freeze command),
-- and EmitInventoryAccountingEvent (Approved→AccountingEventEmitted,
-- collapsing Calculated/Approved the same way AST-02's own
-- ValidateDepreciationRun does) are the only real transitions.
-- Reconciled has no command anywhere and is left unreachable, the same
-- posture as AST-02's own Reconciled/Certified gap.
--
-- Scope narrowing, stated honestly up front: of the eight explicit
-- commands, this v1 builds ValueMovement, CreateValuationRun,
-- EmitInventoryAccountingEvent, RecordInventoryWriteDown and
-- ReverseWriteDown as real, working commands. RecalculateValuation,
-- AllocateLandedCost and RebuildCostLayersControlled are deliberately
-- deferred — each is real, non-trivial follow-on work (recalculating
-- already-FINAL entries safely, allocating a landed cost across many
-- layers, and a controlled full rebuild from movement history) that
-- would roughly double this migration's own scope. The "Method not
-- permitted by active accounting policy blocks valuation" failure
-- semantic is satisfied for free: LIFO was never added to INV-01's own
-- ValuationMethod enum (migration 000001, FIFO/WEIGHTED_AVERAGE/
-- STANDARD_COST only) — there is no code path that could ever value a
-- movement using a prohibited method, because that method does not exist
-- anywhere in this platform. This directly satisfies negative path #1,
-- "IFRS book uses prohibited LIFO profile."
--
-- inventory_cost_layers is the real "InventoryCostLayer/Pool" — one row
-- per INBOUND (RECEIPT/ADJUSTMENT-increase) movement, created exactly
-- once by ValueMovement (UNIQUE(tenant_id, source_movement_id) — also
-- half of negative path #4, "Same movement consumes two cost layers
-- twice," since an inbound movement can only ever create ONE layer).
-- inventory_valuation_entries is the real "ValuationEntry" — exactly one
-- row per movement, ever (UNIQUE(tenant_id, movement_id) — the other
-- half of negative path #4: an OUTBOUND movement can only ever be valued
-- once, so it can only ever consume layers once).
-- inventory_layer_consumptions is the real "cost-layer trace" the spec's
-- own Evidence field names — which layers, and how much of each, a given
-- OUTBOUND valuation entry actually drew from.
CREATE TABLE inventory_cost_layers (
    layer_id                  UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id                VARCHAR(255) NOT NULL,
    item_id                           UUID NOT NULL REFERENCES inventory_items(item_id),
    location_id                        UUID NOT NULL REFERENCES inventory_locations(location_id),
    source_movement_id                   UUID NOT NULL REFERENCES inventory_movements(movement_id),
    original_quantity                      NUMERIC(18,4) NOT NULL,
    remaining_quantity                       NUMERIC(18,4) NOT NULL,
    unit_cost                                  NUMERIC(18,6) NOT NULL,
    created_at                                   TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT chk_cost_layer_remaining_nonnegative CHECK (remaining_quantity >= 0),
    UNIQUE (tenant_id, source_movement_id)
);

CREATE TABLE inventory_valuation_entries (
    entry_id                  UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id                VARCHAR(255) NOT NULL,
    item_id                           UUID NOT NULL REFERENCES inventory_items(item_id),
    location_id                        UUID NOT NULL REFERENCES inventory_locations(location_id),
    movement_id                          UUID NOT NULL REFERENCES inventory_movements(movement_id),
    entry_type                             VARCHAR(10) NOT NULL, -- INBOUND|OUTBOUND
    quantity                                 NUMERIC(18,4) NOT NULL,
    value                                      NUMERIC(18,2) NOT NULL,
    valuation_method                            VARCHAR(30) NOT NULL, -- snapshot of INV-01's current ValuationPolicy at the time
    fiscal_period                                 VARCHAR(20) NOT NULL,
    run_id                                          UUID, -- set once claimed by a valuation run (FK added below, after the run table exists)
    created_at                                        TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                             VARCHAR(255) NOT NULL,

    CONSTRAINT chk_valuation_entry_type CHECK (entry_type IN ('INBOUND', 'OUTBOUND')),
    UNIQUE (tenant_id, movement_id)
);

CREATE TABLE inventory_layer_consumptions (
    consumption_id               UUID PRIMARY KEY,
    tenant_id                       VARCHAR(255) NOT NULL,
    valuation_entry_id                UUID NOT NULL REFERENCES inventory_valuation_entries(entry_id),
    layer_id                             UUID NOT NULL REFERENCES inventory_cost_layers(layer_id),
    quantity_consumed                       NUMERIC(18,4) NOT NULL,
    unit_cost_at_consumption                   NUMERIC(18,6) NOT NULL,
    created_at                                   TIMESTAMP WITH TIME ZONE NOT NULL
);

-- inventory_valuation_runs mirrors AST-02's own DepreciationRun exactly
-- — one batch GL posting per (legal_entity, fiscal_period), covering
-- every FINAL valuation entry in that period not yet claimed by a prior
-- run.
CREATE TABLE inventory_valuation_runs (
    run_id                       UUID PRIMARY KEY,
    tenant_id                      VARCHAR(255) NOT NULL,
    legal_entity_id                   VARCHAR(255) NOT NULL,
    fiscal_period                        VARCHAR(20) NOT NULL,
    inventory_account_code                 VARCHAR(64) NOT NULL, -- caller-supplied, same bootstrap posture as AST-02's own run account codes
    cogs_account_code                        VARCHAR(64) NOT NULL,
    status                                      VARCHAR(30) NOT NULL, -- DRAFT|POPULATION_FROZEN|APPROVED|ACCOUNTING_EVENT_EMITTED
    journal_id                                    VARCHAR(255),
    created_at                                      TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                           VARCHAR(255) NOT NULL,
    approved_at                                         TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id                              VARCHAR(255),
    emitted_at                                              TIMESTAMP WITH TIME ZONE,

    CONSTRAINT chk_valuation_run_status CHECK (status IN ('DRAFT', 'POPULATION_FROZEN', 'APPROVED', 'ACCOUNTING_EVENT_EMITTED'))
);

CREATE UNIQUE INDEX idx_valuation_runs_live_period
    ON inventory_valuation_runs (tenant_id, legal_entity_id, fiscal_period) WHERE status != 'ACCOUNTING_EVENT_EMITTED';

ALTER TABLE inventory_valuation_entries
    ADD CONSTRAINT fk_valuation_entry_run FOREIGN KEY (run_id) REFERENCES inventory_valuation_runs(run_id);

-- inventory_write_downs is RecordInventoryWriteDown/ReverseWriteDown's
-- own authority. Negative path #3, "NRV write-down lacks evidence," is
-- a real NOT NULL column, not merely a documented convention.
CREATE TABLE inventory_write_downs (
    write_down_id             UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id                 VARCHAR(255) NOT NULL,
    item_id                           UUID NOT NULL REFERENCES inventory_items(item_id),
    location_id                        UUID NOT NULL REFERENCES inventory_locations(location_id),
    amount                                NUMERIC(18,2) NOT NULL,
    valuation_evidence_ref                  VARCHAR(255) NOT NULL,
    expense_account_code                      VARCHAR(64) NOT NULL,
    inventory_account_code                      VARCHAR(64) NOT NULL,
    journal_id                                    VARCHAR(255),
    status                                          VARCHAR(30) NOT NULL, -- ACCOUNTING_EVENT_EMITTED|REVERSED
    created_at                                        TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                             VARCHAR(255) NOT NULL,
    reversed_at                                           TIMESTAMP WITH TIME ZONE,
    reversed_by_principal_id                                VARCHAR(255),
    reversal_reason                                           TEXT,

    CONSTRAINT chk_write_down_status CHECK (status IN ('ACCOUNTING_EVENT_EMITTED', 'REVERSED'))
);

ALTER TABLE inventory_cost_layers ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_cost_layers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_cost_layers
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_valuation_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_valuation_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_valuation_entries
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_layer_consumptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_layer_consumptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_layer_consumptions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_valuation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_valuation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_valuation_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_write_downs ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_write_downs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_write_downs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_cost_layers_item_loc ON inventory_cost_layers (tenant_id, item_id, location_id);
CREATE INDEX idx_valuation_entries_item_loc ON inventory_valuation_entries (tenant_id, item_id, location_id);
CREATE INDEX idx_valuation_entries_run ON inventory_valuation_entries (tenant_id, run_id) WHERE run_id IS NOT NULL;
CREATE INDEX idx_layer_consumptions_entry ON inventory_layer_consumptions (tenant_id, valuation_entry_id);
CREATE INDEX idx_write_downs_item_loc ON inventory_write_downs (tenant_id, item_id, location_id);
