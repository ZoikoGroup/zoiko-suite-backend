-- 0025_payroll_run_svc.sql
-- payroll-run-svc → schema `payroll_run`
--
-- Squashed end state of 000001_initial_schema, 000002_add_idempotency_index,
-- 000003_add_finalization_linkage and 000004_payslip_components. Four tables:
-- payroll_runs, pay_slips, pay_slip_items, shadow_payroll_comparisons.
--
-- Depends on 0022 and 0024: the service resolves each employee through
-- employee-master-svc, takes the base salary from employment-contracts-svc, and
-- reads the component composition from compensation-svc.
--
-- ── What 000004 added, and why ───────────────────────────────────────────────
-- A pay slip recorded four totals and nothing else, so nobody could answer "why
-- is my net pay this number". The lines behind the totals now come from
-- compensation-svc's breakdown endpoint and are stored with the slip that used
-- them.
--
-- ── The lines are copied, not referenced ─────────────────────────────────────
-- pay_slip_items carries calculation_method and calculation_value rather than
-- pointing at compensation.structure_components. If a structure changes 40% HRA
-- to 35% next quarter, this payslip must still show the 40% it was actually
-- paid on. A foreign key would make the slip re-read as whatever the structure
-- says today, which is the opposite of what a payslip is for.
--
-- That is also why this schema holds no FK into `compensation` at all:
-- structure_id is recorded as a plain UUID, an audit trail of which structure
-- was used, not a live join.
--
-- ── Finalized runs are immutable ─────────────────────────────────────────────
-- Two triggers below refuse UPDATE and DELETE of a COMPLETED run, and of any
-- slip belonging to one. A trigger rather than a grant or a policy because it
-- binds even a BYPASSRLS role such as Supabase's service_role, which neither of
-- those does.
--
-- They are service-local functions rather than the shared app.reject_mutation()
-- because these tables are not append-only: a run is updated repeatedly while it
-- is being calculated, and its slips are cleared and re-derived on every
-- recalculation. Only mutation AFTER finalization is refused, which is a
-- conditional the shared function does not express.

CREATE SCHEMA IF NOT EXISTS payroll_run;

COMMENT ON SCHEMA payroll_run IS
    'payroll-run-svc. Payroll runs, the payslips they produced and the lines behind each payslip. A COMPLETED run and its slips are immutable, enforced by trigger.';

GRANT USAGE ON SCHEMA payroll_run TO zoiko_backend, authenticated;

-- ── payroll_runs ─────────────────────────────────────────────────────────────

CREATE TABLE payroll_run.payroll_runs (
    run_id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              VARCHAR(255) NOT NULL,
    legal_entity_id        VARCHAR(255) NOT NULL,

    run_number             VARCHAR(100) NOT NULL,
    pay_period_start       DATE         NOT NULL,
    pay_period_end         DATE         NOT NULL,
    pay_date               DATE         NOT NULL,

    -- INITIATED | CALCULATED | BLOCKED | COMPLETED
    status                 VARCHAR(50)  NOT NULL,
    is_shadow_run          BOOLEAN      NOT NULL DEFAULT false,

    total_gross_pay        NUMERIC(18, 4) NOT NULL DEFAULT 0,
    total_net_pay          NUMERIC(18, 4) NOT NULL DEFAULT 0,
    total_tax_deductions   NUMERIC(18, 4) NOT NULL DEFAULT 0,
    total_other_deductions NUMERIC(18, 4) NOT NULL DEFAULT 0,
    employee_count         INT          NOT NULL DEFAULT 0,

    correlation_id         VARCHAR(255) NOT NULL DEFAULT '',

    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    finalized_at           TIMESTAMPTZ,

    -- Populated only when the caller finalizing the run supplies one; no
    -- fabricated value when none exists yet.
    governance_decision_id TEXT,
    -- Computed by the service over the run's own locked totals at the moment of
    -- finalization — not invented, not caller-supplied.
    snapshot_hash          TEXT,

    CONSTRAINT payroll_runs_status_known
        CHECK (status IN ('INITIATED', 'CALCULATED', 'BLOCKED', 'COMPLETED')),

    -- A pay period that ends before it starts is not a period, and the pay date
    -- cannot precede the work it pays for.
    CONSTRAINT payroll_runs_period_ordered
        CHECK (pay_period_end >= pay_period_start),
    CONSTRAINT payroll_runs_pay_date_after_period
        CHECK (pay_date >= pay_period_start),

    CONSTRAINT payroll_runs_totals_non_negative
        CHECK (total_gross_pay >= 0
           AND total_tax_deductions >= 0
           AND total_other_deductions >= 0
           AND employee_count >= 0),

    -- A COMPLETED run must record when it was finalized and the hash that makes
    -- it reproducible. Without both, "finalized" is a status with nothing behind
    -- it and an auditor has only the final numbers to trust.
    CONSTRAINT payroll_runs_completed_is_evidenced
        CHECK (
            status <> 'COMPLETED'
            OR (finalized_at IS NOT NULL AND snapshot_hash IS NOT NULL)
        ),

    CONSTRAINT payroll_runs_tenant_id_unique UNIQUE (tenant_id, run_id)
);

-- Idempotency: a retried initiate (a client timeout on a request that actually
-- succeeded) resolves to the original run instead of creating a duplicate run
-- for the same period.
CREATE UNIQUE INDEX idx_payroll_runs_tenant_correlation
    ON payroll_run.payroll_runs (tenant_id, correlation_id)
    WHERE correlation_id <> '';

CREATE INDEX idx_payroll_runs_tenant_entity
    ON payroll_run.payroll_runs (tenant_id, legal_entity_id);
CREATE INDEX idx_payroll_runs_tenant_status
    ON payroll_run.payroll_runs (tenant_id, status);

-- ── pay_slips ────────────────────────────────────────────────────────────────

CREATE TABLE payroll_run.pay_slips (
    slip_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           VARCHAR(255) NOT NULL,
    run_id              UUID         NOT NULL,

    employee_id         UUID         NOT NULL,
    employee_number     VARCHAR(100) NOT NULL,
    employee_name       VARCHAR(200) NOT NULL,

    gross_pay           NUMERIC(18, 4) NOT NULL,
    tax_withheld        NUMERIC(18, 4) NOT NULL,
    benefits_deductions NUMERIC(18, 4) NOT NULL,
    net_pay             NUMERIC(18, 4) NOT NULL,

    currency            VARCHAR(3)   NOT NULL,
    effective_date      DATE         NOT NULL,

    -- The base tax was actually applied to. Not the same as gross: non-taxable
    -- earnings are excluded and taxable deductions come off it.
    taxable_amount      NUMERIC(18, 4) NOT NULL DEFAULT 0,

    -- The compensation structure this slip was computed from. Nullable: an
    -- employee paid a flat base salary has none, which is a valid state rather
    -- than missing data. Deliberately not a foreign key — see the header.
    structure_id        UUID,

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- A slip in one tenant against another tenant's run is unrepresentable.
    -- The single-column reference in 000001 accepted it.
    CONSTRAINT pay_slips_run_same_tenant
        FOREIGN KEY (tenant_id, run_id)
        REFERENCES payroll_run.payroll_runs (tenant_id, run_id) ON DELETE CASCADE,

    -- Gross, tax and deductions are quantities. Net is not: deductions can
    -- legitimately exceed pay — an advance recovery, say — and reporting that
    -- as anything other than negative would hide it from the payroll manager
    -- who has to act on it.
    CONSTRAINT pay_slips_amounts_non_negative
        CHECK (gross_pay >= 0
           AND tax_withheld >= 0
           AND benefits_deductions >= 0
           AND taxable_amount >= 0),

    -- Tax cannot be withheld on more than the whole gross.
    CONSTRAINT pay_slips_taxable_within_gross
        CHECK (taxable_amount <= gross_pay),

    CONSTRAINT pay_slips_tenant_id_unique UNIQUE (tenant_id, slip_id)
);

CREATE INDEX idx_pay_slips_tenant_run
    ON payroll_run.pay_slips (tenant_id, run_id);
CREATE INDEX idx_pay_slips_tenant_emp
    ON payroll_run.pay_slips (tenant_id, employee_id);

-- ── pay_slip_items ───────────────────────────────────────────────────────────

CREATE TABLE payroll_run.pay_slip_items (
    item_id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          VARCHAR(255) NOT NULL,
    slip_id            UUID         NOT NULL,

    -- Which catalogue component this came from, for tracing. Not a foreign key
    -- into `compensation`: the component may be retired later, and this line
    -- must keep reading the same either way.
    component_id       UUID,

    component_code     VARCHAR(50)  NOT NULL,
    component_name     VARCHAR(150) NOT NULL,
    -- EARNING | DEDUCTION
    component_type     VARCHAR(20)  NOT NULL,
    is_taxable         BOOLEAN      NOT NULL DEFAULT true,

    -- FIXED | PERCENT_OF_BASE — copied, so the slip shows the derivation it was
    -- actually paid on rather than whatever the structure says today.
    calculation_method VARCHAR(30)  NOT NULL,
    calculation_value  NUMERIC(18, 4) NOT NULL,

    amount             NUMERIC(18, 4) NOT NULL,
    sequence           INTEGER      NOT NULL DEFAULT 0,

    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT pay_slip_items_slip_same_tenant
        FOREIGN KEY (tenant_id, slip_id)
        REFERENCES payroll_run.pay_slips (tenant_id, slip_id) ON DELETE CASCADE,

    CONSTRAINT pay_slip_items_type_check
        CHECK (component_type IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT pay_slip_items_method_check
        CHECK (calculation_method IN ('FIXED', 'PERCENT_OF_BASE')),
    CONSTRAINT pay_slip_items_amount_check
        CHECK (amount >= 0),
    CONSTRAINT pay_slip_items_value_check
        CHECK (calculation_value >= 0),
    CONSTRAINT pay_slip_items_percent_range_check
        CHECK (calculation_method <> 'PERCENT_OF_BASE' OR calculation_value <= 100),

    -- One line per component per slip.
    CONSTRAINT pay_slip_items_unique UNIQUE (slip_id, component_code)
);

-- Lines are always read for one slip, in payslip order.
CREATE INDEX idx_pay_slip_items_slip
    ON payroll_run.pay_slip_items (tenant_id, slip_id, sequence);

-- ── shadow_payroll_comparisons ───────────────────────────────────────────────

CREATE TABLE payroll_run.shadow_payroll_comparisons (
    comparison_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           VARCHAR(255) NOT NULL,
    run_id              UUID         NOT NULL,
    employee_id         UUID         NOT NULL,

    legacy_gross_pay    NUMERIC(18, 4) NOT NULL,
    legacy_net_pay      NUMERIC(18, 4) NOT NULL,
    legacy_tax_withheld NUMERIC(18, 4) NOT NULL,

    zoiko_gross_pay     NUMERIC(18, 4) NOT NULL,
    zoiko_net_pay       NUMERIC(18, 4) NOT NULL,
    zoiko_tax_withheld  NUMERIC(18, 4) NOT NULL,

    gross_variance      NUMERIC(18, 4) NOT NULL,
    net_variance        NUMERIC(18, 4) NOT NULL,
    tax_variance        NUMERIC(18, 4) NOT NULL,
    is_equivalent       BOOLEAN      NOT NULL,

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT shadow_comparisons_run_same_tenant
        FOREIGN KEY (tenant_id, run_id)
        REFERENCES payroll_run.payroll_runs (tenant_id, run_id) ON DELETE CASCADE
);

CREATE INDEX idx_shadow_comp_tenant_run
    ON payroll_run.shadow_payroll_comparisons (tenant_id, run_id);

-- ── Finalized runs are immutable ─────────────────────────────────────────────

CREATE OR REPLACE FUNCTION payroll_run.reject_finalized_run_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    -- On UPDATE, OLD is the row as it stands. A run may be updated freely up to
    -- the moment it completes; the transition INTO COMPLETED is itself an
    -- update of a not-yet-completed row, so it passes.
    IF OLD.status = 'COMPLETED' THEN
        RAISE EXCEPTION
            'payroll run % is finalized and immutable: % is not permitted',
            OLD.run_id, TG_OP;
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION payroll_run.reject_finalized_run_mutation() IS
    'Refuses UPDATE and DELETE of a COMPLETED payroll run. A trigger rather than a grant or policy because it binds even a BYPASSRLS role such as service_role.';

CREATE TRIGGER payroll_runs_finalized_immutable
    BEFORE UPDATE OR DELETE ON payroll_run.payroll_runs
    FOR EACH ROW
    EXECUTE FUNCTION payroll_run.reject_finalized_run_mutation();

CREATE OR REPLACE FUNCTION payroll_run.reject_finalized_slip_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    run_status VARCHAR(50);
BEGIN
    SELECT status INTO run_status
    FROM payroll_run.payroll_runs
    WHERE run_id = OLD.run_id;

    IF run_status = 'COMPLETED' THEN
        RAISE EXCEPTION
            'pay slip % belongs to finalized run %: % is not permitted',
            OLD.slip_id, OLD.run_id, TG_OP;
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION payroll_run.reject_finalized_slip_mutation() IS
    'Refuses UPDATE and DELETE of a pay slip belonging to a COMPLETED run. Recalculation deletes and reinserts slips, which must stop once the run is finalized.';

CREATE TRIGGER pay_slips_finalized_immutable
    BEFORE UPDATE OR DELETE ON payroll_run.pay_slips
    FOR EACH ROW
    EXECUTE FUNCTION payroll_run.reject_finalized_slip_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE payroll_run.payroll_runs               ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_run.payroll_runs               FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_run.pay_slips                  ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_run.pay_slips                  FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_run.pay_slip_items             ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_run.pay_slip_items             FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_run.shadow_payroll_comparisons ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_run.shadow_payroll_comparisons FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON payroll_run.payroll_runs
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON payroll_run.pay_slips
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON payroll_run.pay_slip_items
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON payroll_run.shadow_payroll_comparisons
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

-- A payslip is addressed to one person, so the read is scoped to that person
-- rather than to the tenant. This is the same shape as notification-svc's
-- recipient_read and for the same reason.
--
-- It depends on app.current_principal_id() being the employee_id, which is only
-- true once identity-context principals carry it. Until then this policy
-- matches nothing, which fails closed — nobody reads a payslip they should not.
CREATE POLICY own_payslip_read ON payroll_run.pay_slips
    FOR SELECT TO authenticated
    USING (
        tenant_id = app.current_tenant_id()
        AND employee_id::text = app.current_principal_id()
    );

CREATE POLICY own_payslip_items_read ON payroll_run.pay_slip_items
    FOR SELECT TO authenticated
    USING (
        tenant_id = app.current_tenant_id()
        AND EXISTS (
            SELECT 1 FROM payroll_run.pay_slips s
            WHERE s.slip_id = pay_slip_items.slip_id
              AND s.tenant_id = pay_slip_items.tenant_id
              AND s.employee_id::text = app.current_principal_id()
        )
    );

-- ── Grants ───────────────────────────────────────────────────────────────────

-- No DELETE on payroll_runs: a run is the record that payroll was executed for
-- a period, including one that was blocked.
GRANT SELECT, INSERT, UPDATE ON payroll_run.payroll_runs TO zoiko_backend;

-- Slips, their lines and shadow comparisons DO take DELETE: recalculating a run
-- clears and re-derives them inside one transaction. The triggers above stop
-- that once the run is COMPLETED, which is the point at which the numbers stop
-- being provisional.
GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_run.pay_slips                  TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_run.pay_slip_items             TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_run.shadow_payroll_comparisons TO zoiko_backend;

GRANT SELECT ON payroll_run.pay_slips      TO authenticated;
GRANT SELECT ON payroll_run.pay_slip_items TO authenticated;
