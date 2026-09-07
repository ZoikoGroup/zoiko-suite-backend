-- 0024_compensation_svc.sql
-- compensation-svc → schema `compensation`
--
-- Squashed end state of 000001_initial_schema, 000002_fix_race_and_idempotency
-- and 000003_salary_components. Five tables: compensation_structures,
-- wage_revisions, bonus_grants, salary_components, structure_components.
--
-- Depends on 0022: an employee_id here is an employee_master employee.
--
-- ── What 000003 added, and why ───────────────────────────────────────────────
-- A compensation_structure was a min/max band and nothing else, so nothing
-- downstream could answer "what is this salary actually made of". Payroll needs
-- the breakdown: which elements are earnings, which are deductions, which are
-- taxable, and how each is derived from the base. payroll-run-svc reads it
-- through /v1/compensation/structures/{id}/breakdown and puts the resulting
-- lines on the payslip.
--
-- ── Derivations are modelled, not evaluated ──────────────────────────────────
-- zoiko-one, where this was ported from, stored the amount as a free-text
-- `amount_or_formula VARCHAR(255)`. That does not travel: a governance service
-- cannot evaluate arbitrary expressions and still explain a payslip. The two
-- derivations payroll actually needs are declared explicitly instead — a fixed
-- amount, or a percentage of the base.
--
-- Percentages resolve against the base, never against a running total, so the
-- result does not depend on the order the components happen to be stored in.
-- That property lives in the service; the schema keeps the inputs honest.

CREATE SCHEMA IF NOT EXISTS compensation;

COMMENT ON SCHEMA compensation IS
    'compensation-svc. Pay structures and what they are composed of, wage revision history, and bonus grants. The component breakdown is what payroll-run-svc resolves a payslip from.';

GRANT USAGE ON SCHEMA compensation TO zoiko_backend, authenticated;

-- ── compensation_structures ──────────────────────────────────────────────────

CREATE TABLE compensation.compensation_structures (
    structure_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           VARCHAR(255) NOT NULL,
    legal_entity_id     VARCHAR(255) NOT NULL,

    name                VARCHAR(150) NOT NULL,
    -- SALARY | HOURLY
    pay_type            VARCHAR(50)  NOT NULL,

    min_amount          NUMERIC(18, 4) NOT NULL,
    max_amount          NUMERIC(18, 4) NOT NULL,
    currency            VARCHAR(3)   NOT NULL,
    overtime_multiplier NUMERIC(5, 2) NOT NULL DEFAULT 1.50,

    correlation_id      VARCHAR(255) NOT NULL DEFAULT '',

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT comp_structures_pay_type_known
        CHECK (pay_type IN ('SALARY', 'HOURLY')),

    -- An inverted band admits no salary at all, and every later "is this wage
    -- inside its band" check would answer no for every possible amount.
    CONSTRAINT comp_structures_band_ordered
        CHECK (max_amount >= min_amount),
    CONSTRAINT comp_structures_amounts_non_negative
        CHECK (min_amount >= 0),

    -- Overtime paid below the normal rate is not overtime.
    CONSTRAINT comp_structures_overtime_at_least_one
        CHECK (overtime_multiplier >= 1.00),

    CONSTRAINT comp_structures_tenant_id_unique UNIQUE (tenant_id, structure_id)
);

CREATE UNIQUE INDEX idx_comp_struct_tenant_correlation
    ON compensation.compensation_structures (tenant_id, correlation_id)
    WHERE correlation_id <> '';

CREATE INDEX idx_comp_struct_tenant_entity
    ON compensation.compensation_structures (tenant_id, legal_entity_id);

-- ── wage_revisions ───────────────────────────────────────────────────────────

CREATE TABLE compensation.wage_revisions (
    revision_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      VARCHAR(255) NOT NULL,
    employee_id    UUID         NOT NULL,
    structure_id   UUID,

    -- SALARY | HOURLY
    pay_type       VARCHAR(50)  NOT NULL,
    amount         NUMERIC(18, 4) NOT NULL,
    currency       VARCHAR(3)   NOT NULL,

    effective_from DATE         NOT NULL,
    effective_to   DATE,

    reason         TEXT         NOT NULL,
    revised_by     VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),

    -- ACTIVE | SUPERSEDED
    status         VARCHAR(50)  NOT NULL,

    correlation_id VARCHAR(255) NOT NULL DEFAULT '',

    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- A revision against another tenant's structure is unrepresentable.
    CONSTRAINT wage_revisions_structure_same_tenant
        FOREIGN KEY (tenant_id, structure_id)
        REFERENCES compensation.compensation_structures (tenant_id, structure_id),

    CONSTRAINT wage_revisions_pay_type_known
        CHECK (pay_type IN ('SALARY', 'HOURLY')),
    CONSTRAINT wage_revisions_status_known
        CHECK (status IN ('ACTIVE', 'SUPERSEDED')),
    CONSTRAINT wage_revisions_amount_non_negative
        CHECK (amount >= 0),

    -- A revision that ends before it starts was never in force.
    CONSTRAINT wage_revisions_period_ordered
        CHECK (effective_to IS NULL OR effective_to >= effective_from),

    -- The ACTIVE revision is the one in force now, so it has no end date.
    -- Without this, a row could read ACTIVE with an effective_to in the past
    -- and payroll would keep paying it.
    CONSTRAINT wage_revisions_active_is_open
        CHECK (status <> 'ACTIVE' OR effective_to IS NULL)
);

-- At most one ACTIVE wage revision per employee, at the database level. Without
-- it, two concurrent ReviseWage calls could both pass the application's
-- supersede-then-insert sequence and leave two ACTIVE rows — and
-- GetActiveWageRevision (LIMIT 1, no ORDER BY) would hand payroll an arbitrary
-- one of them.
CREATE UNIQUE INDEX idx_wage_revisions_one_active
    ON compensation.wage_revisions (tenant_id, employee_id)
    WHERE status = 'ACTIVE';

CREATE UNIQUE INDEX idx_wage_rev_tenant_correlation
    ON compensation.wage_revisions (tenant_id, correlation_id)
    WHERE correlation_id <> '';

CREATE INDEX idx_wage_rev_tenant_emp
    ON compensation.wage_revisions (tenant_id, employee_id);
CREATE INDEX idx_wage_rev_tenant_status
    ON compensation.wage_revisions (tenant_id, status);

-- ── bonus_grants ─────────────────────────────────────────────────────────────

CREATE TABLE compensation.bonus_grants (
    grant_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      VARCHAR(255) NOT NULL,
    employee_id    UUID         NOT NULL,

    -- PERFORMANCE | ANNUAL | SIGNING | RETENTION
    bonus_type     VARCHAR(50)  NOT NULL,
    amount         NUMERIC(18, 4) NOT NULL,
    currency       VARCHAR(3)   NOT NULL,
    grant_date     DATE         NOT NULL,

    -- PENDING | APPROVED | PAID | CANCELLED
    status         VARCHAR(50)  NOT NULL,
    approved_by    VARCHAR(255),

    correlation_id VARCHAR(255) NOT NULL DEFAULT '',

    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT bonus_grants_type_known
        CHECK (bonus_type IN ('PERFORMANCE', 'ANNUAL', 'SIGNING', 'RETENTION')),
    CONSTRAINT bonus_grants_status_known
        CHECK (status IN ('PENDING', 'APPROVED', 'PAID', 'CANCELLED')),
    CONSTRAINT bonus_grants_amount_non_negative
        CHECK (amount >= 0),

    -- An approved or paid bonus names who approved it. Money that moved with
    -- nobody accountable for the decision is the thing this table exists to
    -- prevent.
    CONSTRAINT bonus_grants_approved_has_approver
        CHECK (status NOT IN ('APPROVED', 'PAID') OR approved_by IS NOT NULL)
);

CREATE UNIQUE INDEX idx_bonus_grants_tenant_correlation
    ON compensation.bonus_grants (tenant_id, correlation_id)
    WHERE correlation_id <> '';

CREATE INDEX idx_bonus_grants_tenant_emp
    ON compensation.bonus_grants (tenant_id, employee_id);
CREATE INDEX idx_bonus_grants_tenant_status
    ON compensation.bonus_grants (tenant_id, status);

-- ── salary_components ────────────────────────────────────────────────────────
--
-- The catalogue of pay elements a legal entity can compose into its structures.

CREATE TABLE compensation.salary_components (
    component_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       VARCHAR(255) NOT NULL,
    legal_entity_id VARCHAR(255) NOT NULL,

    name            VARCHAR(150) NOT NULL,
    -- HRA, TRANSPORT, PF, PROF_TAX, ... Entity-defined, so no CHECK.
    code            VARCHAR(50)  NOT NULL,

    -- EARNING | DEDUCTION
    component_type  VARCHAR(20)  NOT NULL,

    -- Taxable unless stated otherwise. Defaulting the other way would
    -- under-report income by omission.
    is_taxable      BOOLEAN      NOT NULL DEFAULT true,

    default_amount  NUMERIC(18, 4),
    currency        VARCHAR(3)   NOT NULL,
    description     TEXT,

    -- ACTIVE | INACTIVE
    status          VARCHAR(50)  NOT NULL DEFAULT 'ACTIVE',

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT salary_components_type_check
        CHECK (component_type IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT salary_components_status_check
        CHECK (status IN ('ACTIVE', 'INACTIVE')),

    -- A negative component would flip an earning into a deduction by stealth.
    CONSTRAINT salary_components_amount_check
        CHECK (default_amount IS NULL OR default_amount >= 0),

    CONSTRAINT salary_components_tenant_id_unique UNIQUE (tenant_id, component_id)
);

-- A component code is the stable handle payroll refers to, so it is unique
-- within a legal entity. Partial on ACTIVE so a retired code can be reissued.
CREATE UNIQUE INDEX idx_salary_components_entity_code
    ON compensation.salary_components (tenant_id, legal_entity_id, code)
    WHERE status = 'ACTIVE';

CREATE INDEX idx_salary_components_tenant_entity
    ON compensation.salary_components (tenant_id, legal_entity_id, status);

-- ── structure_components ─────────────────────────────────────────────────────
--
-- Which components make up a structure, and how each is derived.

CREATE TABLE compensation.structure_components (
    structure_component_id UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              VARCHAR(255) NOT NULL,
    structure_id           UUID         NOT NULL,
    component_id           UUID         NOT NULL,

    -- FIXED | PERCENT_OF_BASE
    calculation_method     VARCHAR(30)  NOT NULL,
    calculation_value      NUMERIC(18, 4) NOT NULL,

    -- Payslip display order.
    sequence               INTEGER      NOT NULL DEFAULT 0,

    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- Both parents must be in this row's tenant. A structure composed from
    -- another entity's components would produce a payslip that crosses a legal
    -- boundary, and the single-column form would have allowed it.
    CONSTRAINT structure_components_structure_same_tenant
        FOREIGN KEY (tenant_id, structure_id)
        REFERENCES compensation.compensation_structures (tenant_id, structure_id),
    CONSTRAINT structure_components_component_same_tenant
        FOREIGN KEY (tenant_id, component_id)
        REFERENCES compensation.salary_components (tenant_id, component_id),

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

-- Reading a structure's composition is the hot path: payroll does it per
-- employee per run, always in payslip order.
CREATE INDEX idx_structure_components_structure
    ON compensation.structure_components (tenant_id, structure_id, sequence);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE compensation.compensation_structures ENABLE ROW LEVEL SECURITY;
ALTER TABLE compensation.compensation_structures FORCE  ROW LEVEL SECURITY;
ALTER TABLE compensation.wage_revisions          ENABLE ROW LEVEL SECURITY;
ALTER TABLE compensation.wage_revisions          FORCE  ROW LEVEL SECURITY;
ALTER TABLE compensation.bonus_grants            ENABLE ROW LEVEL SECURITY;
ALTER TABLE compensation.bonus_grants            FORCE  ROW LEVEL SECURITY;
ALTER TABLE compensation.salary_components       ENABLE ROW LEVEL SECURITY;
ALTER TABLE compensation.salary_components       FORCE  ROW LEVEL SECURITY;
ALTER TABLE compensation.structure_components    ENABLE ROW LEVEL SECURITY;
ALTER TABLE compensation.structure_components    FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON compensation.compensation_structures
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON compensation.wage_revisions
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON compensation.bonus_grants
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON compensation.salary_components
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON compensation.structure_components
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

-- Nothing here is readable by `authenticated`. Every table in this schema is
-- either an individual's pay or the bands and components that would let one
-- employee derive a colleague's. Reads go through the service, which resolves
-- the caller's own employee_id first.

-- ── Grants ───────────────────────────────────────────────────────────────────

-- wage_revisions and bonus_grants take no DELETE and no UPDATE beyond the
-- supersede/approve transitions the service performs: both are the history of
-- what somebody was paid and who decided it.
GRANT SELECT, INSERT, UPDATE ON compensation.compensation_structures TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON compensation.wage_revisions          TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON compensation.bonus_grants            TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON compensation.salary_components       TO zoiko_backend;

-- structure_components is the exception that needs DELETE: a structure's
-- composition is replaced as a set, and a partial write would leave a structure
-- computing a payslip nobody intended. The service clears and re-inserts inside
-- one transaction.
GRANT SELECT, INSERT, UPDATE, DELETE ON compensation.structure_components TO zoiko_backend;
