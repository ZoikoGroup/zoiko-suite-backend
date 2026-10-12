ALTER TABLE employment_contracts
    DROP COLUMN IF EXISTS ctc,
    DROP COLUMN IF EXISTS basic_salary,
    DROP COLUMN IF EXISTS hra,
    DROP COLUMN IF EXISTS special_allowance,
    DROP COLUMN IF EXISTS conveyance_allowance,
    DROP COLUMN IF EXISTS medical_allowance,
    DROP COLUMN IF EXISTS lta,
    DROP COLUMN IF EXISTS probation_period_days,
    DROP COLUMN IF EXISTS notice_period_days,
    DROP COLUMN IF EXISTS working_hours_per_week,
    DROP COLUMN IF EXISTS shift_type;
