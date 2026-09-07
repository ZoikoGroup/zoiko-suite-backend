-- Compensation — salary components, and the composition of a structure.
--
-- A compensation_structure was a min/max band and nothing else, so nothing
-- downstream could answer "what is this salary actually made of". Payroll needs
-- the breakdown: which elements are earnings, which are deductions, which are
-- taxable, and how each is derived from the base.

CREATE TABLE IF NOT EXISTS salary_components (
    component_id     UUID PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    legal_entity_id  VARCHAR(255) NOT NULL,
    name             VARCHAR(150) NOT NULL,
    code             VARCHAR(50) NOT NULL,  -- HRA, TRANSPORT, PF, PROF_TAX, ...
    component_type   VARCHAR(20) NOT NULL,  -- EARNING, DEDUCTION
    is_taxable       BOOLEAN NOT NULL DEFAULT true,
    default_amount   NUMERIC(18, 4),
    currency         VARCHAR(3) NOT NULL,
    description      TEXT,
    status           VARCHAR(50) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, INACTIVE
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at       TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT salary_components_type_check
        CHECK (component_type IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT salary_components_status_check
        CHECK (status IN ('ACTIVE', 'INACTIVE')),
    -- A negative component would flip an earning into a deduction by stealth.
    CONSTRAINT salary_components_amount_check
        CHECK (default_amount IS NULL OR default_amount >= 0)
);

-- structure_components is the composition of a structure: which components
-- apply, and how each is derived.
--
-- zoiko-one stored this as a free-text amount_or_formula. That is not portable
-- here — a governance service cannot evaluate arbitrary expressions and still
-- explain a payslip. The two derivations payroll actually needs are modelled
-- explicitly instead: a fixed amount, or a percentage of the base.
CREATE TABLE IF NOT EXISTS structure_components (
    structure_component_id UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    structure_id           UUID NOT NULL REFERENCES compensation_structures(structure_id),
    component_id           UUID NOT NULL REFERENCES salary_components(component_id),
    calculation_method     VARCHAR(30) NOT NULL, -- FIXED, PERCENT_OF_BASE
    calculation_value      NUMERIC(18, 4) NOT NULL,
    sequence               INTEGER NOT NULL DEFAULT 0, -- payslip display order
    created_at             TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at             TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT structure_components_method_check
        CHECK (calculation_method IN ('FIXED', 'PERCENT_OF_BASE')),
    CONSTRAINT structure_components_value_check
        CHECK (calculation_value >= 0),
    -- A percentage over 100 of the base is a data-entry slip, not a policy.
    CONSTRAINT structure_components_percent_range_check
        CHECK (calculation_method <> 'PERCENT_OF_BASE' OR calculation_value <= 100),
    -- One line per component per structure; changing the amount updates the row.
    CONSTRAINT structure_components_unique UNIQUE (structure_id, component_id)
);

ALTER TABLE salary_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE structure_components ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_salary_components ON salary_components FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_structure_components ON structure_components FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- A component code is the stable handle payroll refers to, so it must be
-- unique within a legal entity. Partial on ACTIVE so a retired code can be
-- reissued.
CREATE UNIQUE INDEX IF NOT EXISTS idx_salary_components_entity_code
    ON salary_components (tenant_id, legal_entity_id, code)
    WHERE status = 'ACTIVE';

CREATE INDEX IF NOT EXISTS idx_salary_components_tenant_entity
    ON salary_components (tenant_id, legal_entity_id, status);

-- Reading a structure composition is the hot path: payroll does it per employee
-- per run, always ordered for the payslip.
CREATE INDEX IF NOT EXISTS idx_structure_components_structure
    ON structure_components (tenant_id, structure_id, sequence);
