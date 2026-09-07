-- Reverses 000003_leave_policy_and_holidays.

DROP INDEX IF EXISTS idx_holidays_tenant_entity_range;
DROP INDEX IF EXISTS idx_holidays_tenant_entity_date;
DROP POLICY IF EXISTS tenant_isolation_holidays ON holidays;
DROP TABLE IF EXISTS holidays;

DROP INDEX IF EXISTS idx_leave_types_tenant_entity_code;

ALTER TABLE leave_types DROP CONSTRAINT IF EXISTS leave_types_carry_forward_cap_check;
ALTER TABLE leave_types DROP CONSTRAINT IF EXISTS leave_types_notice_non_negative_check;

ALTER TABLE leave_types
    DROP COLUMN IF EXISTS icon,
    DROP COLUMN IF EXISTS color_hex,
    DROP COLUMN IF EXISTS requires_approval,
    DROP COLUMN IF EXISTS max_consecutive_days,
    DROP COLUMN IF EXISTS min_notice_days,
    DROP COLUMN IF EXISTS carry_forward_max_hours,
    DROP COLUMN IF EXISTS carry_forward_allowed;
