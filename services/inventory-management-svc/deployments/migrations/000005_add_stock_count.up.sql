-- Migration: 000005_add_stock_count.up.sql
--
-- INV-05 (Stock Count): "owns StockCountSession. Must never own: Direct
-- on-hand edits or valuation/accounting policy." Fuller ownership
-- (verbatim): "StockCountSession; count population/snapshot; CountLine;
-- observed quantity; recount; variance; reason; approval; adjustment
-- proposal; completion certificate." Purpose (verbatim): "Govern
-- physical/cycle count populations, observations, recounts and variance
-- approvals while requiring all accepted quantity corrections to flow
-- through INV-03 movements." The doc's own cross-reference row states
-- this even more directly: "Count variance is not a direct balance edit
-- — INV-05 approves variance; INV-03 creates the adjustment movement;
-- INV-04 values it; ACC-04 posts any accounting consequence."
--
-- State model (verbatim): "Planned→PopulationFrozen→Counting→
-- VarianceReview→Recount/Approved→AdjustmentsGenerated→Reconciled→
-- Certified; Cancelled before certification." The same doc-vs-command
-- mismatch pattern recurring throughout this build hits INV-05 too:
-- there is no command that reaches "Counting" independently of
-- RecordBlindCount itself (recording the first observation IS what
-- moves a line into Counting — no separate "start counting" command),
-- and "Reconciled" has no command anywhere and is left unreachable, the
-- same posture as AST-02's own Reconciled/Certified gap (CertifyStockCount
-- here reaches Certified directly from AdjustmentsGenerated).
--
-- Minimum negative-path certification (verbatim, all four):
--   1. "Counter sees system quantity in blind count" — RecordBlindCount's
--      own response never includes system_quantity at all (a compile-time
--      absent field, not a runtime redaction) — see the handler's own
--      response struct. GetCountLines is a separate, distinctly-authorized
--      query (inventory.count.approve/read) a counter recording blind
--      observations has no reason to be granted.
--   2. "Population changes after freeze without invalidation" —
--      inventory_stock_count_lines' own economic fields (item_id,
--      location_id, system_quantity) are set exactly once, at freeze
--      time, and trg_reject_count_line_population_mutation blocks any
--      later UPDATE to them — a real, permanent snapshot, not a
--      re-queried live view.
--   3. "Variance approved by same counter" — ApproveCountVariance
--      refuses if the approving principal equals that SPECIFIC line's
--      own observed_by_principal_id — maker/checker at the line level,
--      not merely the whole count session.
--   4. "Count directly overwrites on-hand quantity" — GenerateAdjustmentMovements
--      is the ONLY path from an approved variance to an on-hand change,
--      and it works by calling INV-03's own CreateInventoryMovement/
--      ValidateMovement/CommitMovement in-process (an ADJUSTMENT
--      movement, direction from the variance's own sign) — this
--      migration's own tables have no column that could hold an on-hand
--      quantity at all; adjustment_movement_id is the only link, always
--      to a real row in inventory_movements.
--
-- inventory_stock_count_lines is the real "CountLine" authority — one
-- row per (item, location) in the frozen population, created once by
-- FreezeCountPopulation. inventory_stock_count_locations is the caller's
-- own declared scope (CreateStockCount) — which locations this session
-- covers; the actual item population is derived from real movement
-- history at freeze time, not declared by the caller.
CREATE TABLE inventory_stock_counts (
    count_id                  UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id                VARCHAR(255) NOT NULL,
    fiscal_period                    VARCHAR(20) NOT NULL, -- passed through to any generated adjustment movement
    status                              VARCHAR(30) NOT NULL, -- PLANNED|POPULATION_FROZEN|COUNTING|VARIANCE_REVIEW|ADJUSTMENTS_GENERATED|CERTIFIED|CANCELLED
    cutoff_at                            TIMESTAMP WITH TIME ZONE, -- "count date/cutoff" — set at freeze time
    created_at                             TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                  VARCHAR(255) NOT NULL,
    frozen_at                                  TIMESTAMP WITH TIME ZONE,
    certified_at                                 TIMESTAMP WITH TIME ZONE,
    certified_by_principal_id                      VARCHAR(255),
    cancelled_at                                     TIMESTAMP WITH TIME ZONE,
    cancelled_by_principal_id                          VARCHAR(255),
    cancel_reason                                        TEXT,

    CONSTRAINT chk_stock_count_status CHECK (status IN (
        'PLANNED', 'POPULATION_FROZEN', 'COUNTING', 'VARIANCE_REVIEW', 'ADJUSTMENTS_GENERATED', 'CERTIFIED', 'CANCELLED'
    ))
);

CREATE TABLE inventory_stock_count_locations (
    count_id       UUID NOT NULL REFERENCES inventory_stock_counts(count_id),
    location_id      UUID NOT NULL REFERENCES inventory_locations(location_id),
    PRIMARY KEY (count_id, location_id)
);

CREATE TABLE inventory_stock_count_lines (
    line_id                       UUID PRIMARY KEY,
    tenant_id                       VARCHAR(255) NOT NULL,
    count_id                          UUID NOT NULL REFERENCES inventory_stock_counts(count_id),
    item_id                             UUID NOT NULL REFERENCES inventory_items(item_id),
    location_id                          UUID NOT NULL REFERENCES inventory_locations(location_id),
    system_quantity                        NUMERIC(18,4) NOT NULL, -- frozen at snapshot time — see negative path #2
    assigned_counter_principal_id            VARCHAR(255),
    observed_quantity                          NUMERIC(18,4),
    observed_at                                  TIMESTAMP WITH TIME ZONE,
    observed_by_principal_id                       VARCHAR(255),
    status                                           VARCHAR(30) NOT NULL, -- PENDING|COUNTED|NEEDS_RECOUNT|VARIANCE_APPROVED|ADJUSTMENT_GENERATED
    variance_approved_at                               TIMESTAMP WITH TIME ZONE,
    variance_approved_by_principal_id                    VARCHAR(255),
    adjustment_movement_id                                 UUID REFERENCES inventory_movements(movement_id),
    created_at                                               TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT chk_count_line_status CHECK (status IN (
        'PENDING', 'COUNTED', 'NEEDS_RECOUNT', 'VARIANCE_APPROVED', 'ADJUSTMENT_GENERATED'
    )),
    UNIQUE (tenant_id, count_id, item_id, location_id)
);

-- Negative path #2, "Population changes after freeze without
-- invalidation" — item_id, location_id and system_quantity are the
-- population snapshot itself; once written at freeze time they can
-- never change again, regardless of application-code bugs.
CREATE OR REPLACE FUNCTION reject_count_line_population_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.item_id IS DISTINCT FROM OLD.item_id OR
       NEW.location_id IS DISTINCT FROM OLD.location_id OR
       NEW.system_quantity IS DISTINCT FROM OLD.system_quantity THEN
        RAISE EXCEPTION 'inventory_stock_count_lines: the frozen population snapshot (item_id, location_id, system_quantity) is immutable once written';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_count_line_population_mutation
    BEFORE UPDATE ON inventory_stock_count_lines
    FOR EACH ROW EXECUTE FUNCTION reject_count_line_population_mutation();

ALTER TABLE inventory_stock_counts ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_stock_counts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_stock_counts
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_stock_count_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_stock_count_locations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_stock_count_locations
    FOR ALL USING (count_id IN (SELECT count_id FROM inventory_stock_counts WHERE tenant_id = current_setting('app.tenant_id', true)))
    WITH CHECK (count_id IN (SELECT count_id FROM inventory_stock_counts WHERE tenant_id = current_setting('app.tenant_id', true)));

ALTER TABLE inventory_stock_count_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_stock_count_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_stock_count_lines
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_stock_counts_entity ON inventory_stock_counts (tenant_id, legal_entity_id);
CREATE INDEX idx_stock_count_lines_count ON inventory_stock_count_lines (tenant_id, count_id);
