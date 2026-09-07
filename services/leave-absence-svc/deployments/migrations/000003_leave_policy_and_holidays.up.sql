-- Leave & Absence — configurable leave policy, and a holiday calendar.
--
-- Before this migration a leave type carried only an accrual rate and a cap, so
-- every request was reviewed by hand and nothing stopped someone booking six
-- months of leave starting tomorrow. These columns let a legal entity express
-- its actual policy, and the service enforce it at submission time.

ALTER TABLE leave_types
    -- Unused balance rolling into the next leave year, and the cap on it.
    ADD COLUMN IF NOT EXISTS carry_forward_allowed   BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS carry_forward_max_hours NUMERIC(10, 2) NOT NULL DEFAULT 0.00,

    -- Minimum days between submitting a request and the leave starting.
    -- 0 means leave of this type may be booked same-day or retroactively,
    -- which is what sick leave needs.
    ADD COLUMN IF NOT EXISTS min_notice_days INTEGER NOT NULL DEFAULT 0,

    -- Longest unbroken span, in calendar days, a single request may cover.
    -- 0 means unlimited.
    ADD COLUMN IF NOT EXISTS max_consecutive_days INTEGER NOT NULL DEFAULT 0,

    -- false auto-approves on submission. Deliberately defaults to true so an
    -- existing leave type never silently stops being reviewed.
    ADD COLUMN IF NOT EXISTS requires_approval BOOLEAN NOT NULL DEFAULT true,

    -- Display metadata, carried for the caller. The service never reads these.
    ADD COLUMN IF NOT EXISTS color_hex VARCHAR(7),
    ADD COLUMN IF NOT EXISTS icon      VARCHAR(50);

ALTER TABLE leave_types
    DROP CONSTRAINT IF EXISTS leave_types_notice_non_negative_check;
ALTER TABLE leave_types
    ADD CONSTRAINT leave_types_notice_non_negative_check
    CHECK (min_notice_days >= 0 AND max_consecutive_days >= 0 AND carry_forward_max_hours >= 0);

-- Carrying forward with a zero cap would silently discard the whole balance at
-- year end, which reads as a policy bug rather than a policy.
ALTER TABLE leave_types
    DROP CONSTRAINT IF EXISTS leave_types_carry_forward_cap_check;
ALTER TABLE leave_types
    ADD CONSTRAINT leave_types_carry_forward_cap_check
    CHECK (NOT carry_forward_allowed OR carry_forward_max_hours > 0);

-- A leave type code is the stable handle a caller configures policy against, so
-- it must not repeat within a legal entity.
CREATE UNIQUE INDEX IF NOT EXISTS idx_leave_types_tenant_entity_code
    ON leave_types (tenant_id, legal_entity_id, code);


-- ── Holiday calendar ──────────────────────────────────────────────────────────
--
-- Public and company holidays per legal entity. A leave request spanning a
-- holiday should not consume balance for that day, and until now there was
-- nowhere to record which days those are.

CREATE TABLE IF NOT EXISTS holidays (
    holiday_id      UUID PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    legal_entity_id VARCHAR(255) NOT NULL,
    name            VARCHAR(150) NOT NULL,
    holiday_date    DATE NOT NULL,
    holiday_type    VARCHAR(50) NOT NULL DEFAULT 'PUBLIC', -- PUBLIC, COMPANY, OPTIONAL
    is_recurring    BOOLEAN NOT NULL DEFAULT false,        -- same calendar day every year
    description     TEXT,
    status          VARCHAR(50) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, INACTIVE
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at      TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT holidays_type_check
        CHECK (holiday_type IN ('PUBLIC', 'COMPANY', 'OPTIONAL')),
    CONSTRAINT holidays_status_check
        CHECK (status IN ('ACTIVE', 'INACTIVE'))
);

ALTER TABLE holidays ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_holidays ON holidays FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- One entry per day per entity. Partial on ACTIVE so a day can be retired and
-- then re-declared without tripping over the old row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_holidays_tenant_entity_date
    ON holidays (tenant_id, legal_entity_id, holiday_date)
    WHERE status = 'ACTIVE';

-- The calendar is read by date range far more often than by anything else.
CREATE INDEX IF NOT EXISTS idx_holidays_tenant_entity_range
    ON holidays (tenant_id, legal_entity_id, holiday_date, status);
