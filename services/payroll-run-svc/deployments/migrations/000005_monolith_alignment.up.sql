-- Monolith alignment: payroll runs gained statutory contribution totals and
-- batch/approval provenance, and pay slips gained the full earnings/
-- deduction breakdown behind the four headline totals (services/payroll-run-svc
-- internal/store/pg_store.go). The columns were added to the Go code without
-- a migration, so CreatePayrollRun/SaveCalculatedResults failed with 42703
-- (column does not exist).

-- Run totals: every field is a *float64 pointer in Go and a run is created
-- BEFORE its numbers are computed, so the contribution/batch/approval columns
-- must be nullable — a NOT NULL column rejects the NULL the store passes for
-- "not yet calculated".
ALTER TABLE payroll_runs
    ADD COLUMN total_employer_contributions NUMERIC(18, 4),
    ADD COLUMN total_employee_contributions NUMERIC(18, 4),
    ADD COLUMN total_tds                    NUMERIC(18, 4),
    ADD COLUMN total_pf                     NUMERIC(18, 4),
    ADD COLUMN total_esi                    NUMERIC(18, 4),
    ADD COLUMN total_pt                     NUMERIC(18, 4),
    ADD COLUMN batch_id                     VARCHAR(100),
    ADD COLUMN processed_by                 VARCHAR(255),
    ADD COLUMN approved_by                  VARCHAR(255),
    ADD COLUMN approved_at                  TIMESTAMP WITH TIME ZONE;

-- Slip detail: the per-line numbers a payslip is computed from. All are
-- *float64 pointers in Go and a contract rarely populates every breakdown
-- field (e.g. no HRA, no loan), so a missing field is passed as NULL —
-- nullable keeps SaveCalculatedResults' explicit-NULL INSERTs legal.
-- Control-population fixtures omit them entirely, which reads back as NULL.
ALTER TABLE pay_slips
    ADD COLUMN basic_salary        NUMERIC(18, 4),
    ADD COLUMN hra                 NUMERIC(18, 4),
    ADD COLUMN special_allowance   NUMERIC(18, 4),
    ADD COLUMN conveyance          NUMERIC(18, 4),
    ADD COLUMN medical_allowance   NUMERIC(18, 4),
    ADD COLUMN lta                 NUMERIC(18, 4),
    ADD COLUMN pf_employee         NUMERIC(18, 4),
    ADD COLUMN pf_employer         NUMERIC(18, 4),
    ADD COLUMN esi_employee        NUMERIC(18, 4),
    ADD COLUMN esi_employer        NUMERIC(18, 4),
    ADD COLUMN pt                  NUMERIC(18, 4),
    ADD COLUMN tds                 NUMERIC(18, 4),
    ADD COLUMN other_deductions    NUMERIC(18, 4),
    ADD COLUMN arrears             NUMERIC(18, 4),
    ADD COLUMN bonus               NUMERIC(18, 4),
    ADD COLUMN overtime_pay        NUMERIC(18, 4),
    ADD COLUMN leave_encashment    NUMERIC(18, 4),
    ADD COLUMN reimbursements      NUMERIC(18, 4),
    ADD COLUMN advance_deduction   NUMERIC(18, 4),
    ADD COLUMN loan_deduction      NUMERIC(18, 4);
