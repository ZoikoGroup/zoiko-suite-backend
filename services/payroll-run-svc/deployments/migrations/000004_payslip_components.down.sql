-- Reverses 000004_payslip_components.

DROP INDEX IF EXISTS idx_pay_slip_items_slip;
DROP POLICY IF EXISTS tenant_isolation_pay_slip_items ON pay_slip_items;
DROP TABLE IF EXISTS pay_slip_items;

ALTER TABLE pay_slips
    DROP COLUMN IF EXISTS structure_id,
    DROP COLUMN IF EXISTS taxable_amount;
