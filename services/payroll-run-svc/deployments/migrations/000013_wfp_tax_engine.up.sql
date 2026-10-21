-- ZS-SVC-L-001 gap work — tax engine provenance on a payslip.
--
-- When a payroll calculation takes its tax from payroll-tax-svc, each payslip keeps the engine's own
-- breakdown: one row per tax component with the calculation id, engine type, rule version and the
-- jurisdiction it was computed under. WFP records and classifies these; it never recomputes them. A
-- payslip with tax and no rows here was computed by the legacy placeholder rate (and is labelled so when
-- promoted to a WFP result).
--
-- Rows cascade with their slip: recalculating a run replaces its slips and therefore its tax lines.

CREATE TABLE IF NOT EXISTS pay_slip_tax_lines (
    line_id           UUID PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    slip_id           UUID NOT NULL REFERENCES pay_slips(slip_id) ON DELETE CASCADE,
    sequence          INTEGER NOT NULL,
    tax_name          VARCHAR(150) NOT NULL,
    tax_type          VARCHAR(50)  NOT NULL,
    rate_pct          NUMERIC(9, 4) NOT NULL,
    amount            NUMERIC(18, 4) NOT NULL,
    calculation_id    VARCHAR(100) NOT NULL,
    engine_type       VARCHAR(50)  NOT NULL,
    rule_version      VARCHAR(100) NOT NULL,
    jurisdiction_code VARCHAR(100) NOT NULL,
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL,
    CONSTRAINT pay_slip_tax_lines_amount_check CHECK (amount >= 0),
    CONSTRAINT pay_slip_tax_lines_text_check CHECK (length(btrim(tax_name)) > 0 AND length(btrim(tax_type)) > 0
        AND length(btrim(calculation_id)) > 0 AND length(btrim(rule_version)) > 0),
    CONSTRAINT pay_slip_tax_lines_unique UNIQUE (slip_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_pay_slip_tax_lines_slip ON pay_slip_tax_lines (slip_id);

ALTER TABLE pay_slip_tax_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE pay_slip_tax_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_pay_slip_tax_lines ON pay_slip_tax_lines FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
