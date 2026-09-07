-- Payroll Run — per-line payslip detail, and a real taxable base.
--
-- A pay slip recorded four totals and nothing else, so nobody could answer
-- "why is my net pay this number". The lines behind those totals now come from
-- compensation-svc, and are stored with the slip that used them: a payslip must
-- stay explicable even after the structure it was computed from is changed.

-- The base the tax figure was actually applied to. Not the same as gross:
-- non-taxable earnings are excluded, and taxable deductions come off it.
ALTER TABLE pay_slips
    ADD COLUMN IF NOT EXISTS taxable_amount NUMERIC(18, 4) NOT NULL DEFAULT 0,
    -- The compensation structure this slip was computed from, if any. Nullable:
    -- an employee paid a flat base salary has no structure, which is a valid
    -- state rather than missing data.
    ADD COLUMN IF NOT EXISTS structure_id UUID;

CREATE TABLE IF NOT EXISTS pay_slip_items (
    item_id            UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    slip_id            UUID NOT NULL REFERENCES pay_slips(slip_id) ON DELETE CASCADE,
    component_id       UUID,
    component_code     VARCHAR(50) NOT NULL,
    component_name     VARCHAR(150) NOT NULL,
    component_type     VARCHAR(20) NOT NULL, -- EARNING, DEDUCTION
    is_taxable         BOOLEAN NOT NULL DEFAULT true,
    calculation_method VARCHAR(30) NOT NULL, -- FIXED, PERCENT_OF_BASE
    calculation_value  NUMERIC(18, 4) NOT NULL,
    amount             NUMERIC(18, 4) NOT NULL,
    sequence           INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT pay_slip_items_type_check
        CHECK (component_type IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT pay_slip_items_method_check
        CHECK (calculation_method IN ('FIXED', 'PERCENT_OF_BASE')),
    CONSTRAINT pay_slip_items_amount_check
        CHECK (amount >= 0),
    -- One line per component per slip.
    CONSTRAINT pay_slip_items_unique UNIQUE (slip_id, component_code)
);

-- calculation_method and calculation_value are copied rather than referenced on
-- purpose. If a structure changes 40% HRA to 35% next quarter, last quarter's
-- payslip must still show the 40% it was actually paid on.

ALTER TABLE pay_slip_items ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_pay_slip_items ON pay_slip_items FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Lines are always read for one slip, in payslip order.
CREATE INDEX IF NOT EXISTS idx_pay_slip_items_slip
    ON pay_slip_items (tenant_id, slip_id, sequence);
