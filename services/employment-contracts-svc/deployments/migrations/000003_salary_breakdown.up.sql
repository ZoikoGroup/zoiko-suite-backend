-- Monolith alignment: contract issue/amend gained the full CTC breakdown
-- and working-conditions fields (services/employment-contracts-svc
-- internal/store/pg_store.go INSERT lists them), but the columns were only
-- ever added to the Go code, so every IssueContract failed with 42703
-- (column does not exist). Nullable throughout: the Go types are pointers
-- and a contract without a stated breakdown is a legitimate state.

ALTER TABLE employment_contracts
    ADD COLUMN ctc                   NUMERIC(18, 4),
    ADD COLUMN basic_salary          NUMERIC(18, 4),
    ADD COLUMN hra                   NUMERIC(18, 4),
    ADD COLUMN special_allowance     NUMERIC(18, 4),
    ADD COLUMN conveyance_allowance  NUMERIC(18, 4),
    ADD COLUMN medical_allowance     NUMERIC(18, 4),
    ADD COLUMN lta                   NUMERIC(18, 4),
    ADD COLUMN probation_period_days INT,
    ADD COLUMN notice_period_days    INT,
    ADD COLUMN working_hours_per_week NUMERIC(5, 2),
    ADD COLUMN shift_type            VARCHAR(30);
