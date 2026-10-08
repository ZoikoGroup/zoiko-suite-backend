-- Monolith alignment: payroll runs gained statutory contribution totals and
-- batch/approval provenance, and pay slips gained the full earnings/
-- deduction breakdown behind the four headline totals (services/payroll-run-svc
-- internal/store/pg_store.go). The columns were added to the Go code without
-- a migration, so CreatePayrollRun/SaveCalculatedResults failed with 42703
-- (column does not exist).

-- Run totals: nullable pointers in Go, but NOT NULL DEFAULT 0 matches the
-- existing total_gross_pay/total_net_pay convention and keeps a run's
-- totals always summable. batch/processed/approval columns are nullable:
-- a run may have no batch and may never be formally approved.
ALTER TABLE payroll_runs
    ADD COLUMN total_employer_contributions NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN total_employee_contributions NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN total_tds                    NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN total_pf                     NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN total_esi                    NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN total_pt                     NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN batch_id                     VARCHAR(100),
    ADD COLUMN processed_by                 VARCHAR(255),
    ADD COLUMN approved_by                  VARCHAR(255),
    ADD COLUMN approved_at                  TIMESTAMP WITH TIME ZONE;

-- Slip detail: the per-line numbers a payslip is computed from. Amounts are
-- NOT NULL DEFAULT 0 so a slip inserted without them (e.g. a control
-- population fixture) still reads back as zero rather than failing a Scan
-- or forcing every INSERT to enumerate twenty columns.
ALTER TABLE pay_slips
    ADD COLUMN basic_salary        NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN hra                 NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN special_allowance   NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN conveyance          NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN medical_allowance   NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN lta                 NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN pf_employee         NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN pf_employer         NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN esi_employee        NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN esi_employer        NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN pt                  NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN tds                 NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN other_deductions    NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN arrears             NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN bonus               NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN overtime_pay        NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN leave_encashment    NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN reimbursements      NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN advance_deduction   NUMERIC(18, 4) NOT NULL DEFAULT 0,
    ADD COLUMN loan_deduction      NUMERIC(18, 4) NOT NULL DEFAULT 0;
