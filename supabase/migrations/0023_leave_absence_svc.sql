-- 0023_leave_absence_svc.sql
-- leave-absence-svc → schema `leave_absence`
--
-- Squashed end state of 000001_initial_schema, 000002_fix_race_and_idempotency
-- and 000003_leave_policy_and_holidays. Four tables: leave_types,
-- leave_balances, leave_requests, holidays.
--
-- Depends on 0022: an employee_id here is an employee_master employee, and the
-- service resolves the employee's legal entity through that service before it
-- authorizes anything.
--
-- ── Leave policy is enforced, not advisory ───────────────────────────────────
-- Before 000003 a leave type carried an accrual rate and a cap and nothing
-- else, so every request was reviewed by hand and nothing stopped someone
-- booking six months starting tomorrow. min_notice_days, max_consecutive_days
-- and requires_approval are checked by the service at submission time.
--
-- requires_approval DEFAULTs true. A leave type that arrives without the column
-- set must not silently stop being reviewed.
--
-- ── Holidays are retired, never deleted ──────────────────────────────────────
-- Leave approved against last year's calendar has to stay explicable, so a
-- holiday moves to INACTIVE and the row stays. The uniqueness index is partial
-- on ACTIVE so a retired date can be declared again.

CREATE SCHEMA IF NOT EXISTS leave_absence;

COMMENT ON SCHEMA leave_absence IS
    'leave-absence-svc. Leave types and their policy, per-employee balances, requests, and the holiday calendar. Policy on a leave type is enforced at submission, not advisory.';

GRANT USAGE ON SCHEMA leave_absence TO zoiko_backend, authenticated;

-- ── leave_types ──────────────────────────────────────────────────────────────

CREATE TABLE leave_absence.leave_types (
    leave_type_id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,

    name                    VARCHAR(255) NOT NULL,
    -- VACATION | SICK_LEAVE | MATERNITY | PATERNITY | BEREAVEMENT | UNPAID | ...
    -- Deliberately not a CHECK: a legal entity may define its own statutory
    -- leave types, and the set is jurisdiction-dependent.
    code                    VARCHAR(50)  NOT NULL,

    is_paid                 BOOLEAN      NOT NULL DEFAULT true,
    accrual_rate_per_year   NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    max_balance             NUMERIC(10, 2) NOT NULL DEFAULT 0.00,

    -- ACTIVE | INACTIVE
    status                  VARCHAR(50)  NOT NULL,

    -- ── Policy ──────────────────────────────────────────────────────────────
    carry_forward_allowed   BOOLEAN      NOT NULL DEFAULT false,
    carry_forward_max_hours NUMERIC(10, 2) NOT NULL DEFAULT 0.00,

    -- Minimum days between submitting and the leave starting. 0 permits
    -- same-day and retroactive booking, which is what sick leave needs.
    min_notice_days         INTEGER      NOT NULL DEFAULT 0,

    -- Longest unbroken span, in calendar days, one request may cover.
    -- 0 means unlimited.
    max_consecutive_days    INTEGER      NOT NULL DEFAULT 0,

    -- false auto-approves on submission.
    requires_approval       BOOLEAN      NOT NULL DEFAULT true,

    -- Display metadata carried for the caller. The service never reads these.
    color_hex               VARCHAR(7),
    icon                    VARCHAR(50),

    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT leave_types_status_known
        CHECK (status IN ('ACTIVE', 'INACTIVE')),

    CONSTRAINT leave_types_notice_non_negative_check
        CHECK (min_notice_days >= 0
           AND max_consecutive_days >= 0
           AND carry_forward_max_hours >= 0
           AND accrual_rate_per_year >= 0
           AND max_balance >= 0),

    -- Carrying forward with a zero cap would silently discard the whole balance
    -- at year end, which reads as a policy bug rather than a policy.
    CONSTRAINT leave_types_carry_forward_cap_check
        CHECK (NOT carry_forward_allowed OR carry_forward_max_hours > 0),

    CONSTRAINT leave_types_tenant_id_unique UNIQUE (tenant_id, leave_type_id)
);

-- A leave type code is the stable handle policy is configured against, so it
-- must not repeat within a legal entity.
CREATE UNIQUE INDEX idx_leave_types_tenant_entity_code
    ON leave_absence.leave_types (tenant_id, legal_entity_id, code);

CREATE INDEX idx_leave_types_tenant_entity
    ON leave_absence.leave_types (tenant_id, legal_entity_id);

-- ── leave_balances ───────────────────────────────────────────────────────────

CREATE TABLE leave_absence.leave_balances (
    balance_id      UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       VARCHAR(255) NOT NULL,
    employee_id     UUID         NOT NULL,
    leave_type_id   UUID         NOT NULL,

    allocated_hours NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    used_hours      NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    pending_hours   NUMERIC(10, 2) NOT NULL DEFAULT 0.00,

    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- A balance in one tenant against another tenant's leave type is
    -- unrepresentable. The single-column reference in 000001 accepted it.
    CONSTRAINT leave_balances_type_same_tenant
        FOREIGN KEY (tenant_id, leave_type_id)
        REFERENCES leave_absence.leave_types (tenant_id, leave_type_id),

    CONSTRAINT unique_tenant_emp_leave_type
        UNIQUE (tenant_id, employee_id, leave_type_id),

    -- Hours are quantities, never negative. Used and pending hours that exceed
    -- what was allocated would report an available balance below zero, which
    -- the service treats as insufficient and would then never let anyone spend.
    CONSTRAINT leave_balances_hours_non_negative
        CHECK (allocated_hours >= 0 AND used_hours >= 0 AND pending_hours >= 0)
);

CREATE INDEX idx_leave_balances_tenant_emp
    ON leave_absence.leave_balances (tenant_id, employee_id);

-- ── leave_requests ───────────────────────────────────────────────────────────

CREATE TABLE leave_absence.leave_requests (
    request_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      VARCHAR(255) NOT NULL,
    employee_id    UUID         NOT NULL,
    leave_type_id  UUID         NOT NULL,

    start_date     DATE         NOT NULL,
    end_date       DATE         NOT NULL,
    total_hours    NUMERIC(10, 2) NOT NULL,
    reason         TEXT,

    -- SUBMITTED | APPROVED | REJECTED | CANCELLED
    status         VARCHAR(50)  NOT NULL,

    reviewer_id    VARCHAR(255),
    reviewer_notes TEXT,
    reviewed_at    TIMESTAMPTZ,

    correlation_id VARCHAR(255) NOT NULL DEFAULT '',

    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT leave_requests_type_same_tenant
        FOREIGN KEY (tenant_id, leave_type_id)
        REFERENCES leave_absence.leave_types (tenant_id, leave_type_id),

    CONSTRAINT leave_requests_status_known
        CHECK (status IN ('SUBMITTED', 'APPROVED', 'REJECTED', 'CANCELLED')),

    -- Leave that ends before it starts is not a span. The service checks this;
    -- nothing else did.
    CONSTRAINT leave_requests_end_after_start
        CHECK (end_date >= start_date),

    CONSTRAINT leave_requests_hours_positive
        CHECK (total_hours > 0),

    -- A reviewed request must record who reviewed it and when. A row reading
    -- APPROVED with no reviewer is an approval nobody is accountable for.
    CONSTRAINT leave_requests_reviewed_has_reviewer
        CHECK (
            status IN ('SUBMITTED', 'CANCELLED')
            OR (reviewer_id IS NOT NULL AND reviewed_at IS NOT NULL)
        )
);

-- Idempotency: a retried submit resolves to the original request instead of
-- creating a duplicate and double-locking pending hours.
CREATE UNIQUE INDEX idx_leave_requests_tenant_correlation
    ON leave_absence.leave_requests (tenant_id, correlation_id)
    WHERE correlation_id <> '';

CREATE INDEX idx_leave_requests_tenant_emp
    ON leave_absence.leave_requests (tenant_id, employee_id);
CREATE INDEX idx_leave_requests_tenant_status
    ON leave_absence.leave_requests (tenant_id, status);

-- ── holidays ─────────────────────────────────────────────────────────────────
--
-- Public and company holidays per legal entity. Leave spanning a holiday should
-- not consume balance for that day, and before 000003 there was nowhere to
-- record which days those are.

CREATE TABLE leave_absence.holidays (
    holiday_id      UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       VARCHAR(255) NOT NULL,
    legal_entity_id VARCHAR(255) NOT NULL,

    name            VARCHAR(150) NOT NULL,
    holiday_date    DATE         NOT NULL,

    -- PUBLIC | COMPANY | OPTIONAL
    holiday_type    VARCHAR(50)  NOT NULL DEFAULT 'PUBLIC',

    -- Same calendar day every year.
    is_recurring    BOOLEAN      NOT NULL DEFAULT false,
    description     TEXT,

    -- ACTIVE | INACTIVE
    status          VARCHAR(50)  NOT NULL DEFAULT 'ACTIVE',

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT holidays_type_check
        CHECK (holiday_type IN ('PUBLIC', 'COMPANY', 'OPTIONAL')),
    CONSTRAINT holidays_status_check
        CHECK (status IN ('ACTIVE', 'INACTIVE'))
);

-- One entry per day per entity. Partial on ACTIVE so a day can be retired and
-- then re-declared without tripping over the old row.
CREATE UNIQUE INDEX idx_holidays_tenant_entity_date
    ON leave_absence.holidays (tenant_id, legal_entity_id, holiday_date)
    WHERE status = 'ACTIVE';

-- The calendar is read by date range far more often than by anything else.
CREATE INDEX idx_holidays_tenant_entity_range
    ON leave_absence.holidays (tenant_id, legal_entity_id, holiday_date, status);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE leave_absence.leave_types    ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_absence.leave_types    FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_absence.leave_balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_absence.leave_balances FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_absence.leave_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_absence.leave_requests FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_absence.holidays       ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_absence.holidays       FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON leave_absence.leave_types
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON leave_absence.leave_balances
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON leave_absence.leave_requests
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON leave_absence.holidays
    FOR ALL TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

-- Leave types and the holiday calendar are policy an employee is entitled to
-- read: they say how much notice a request needs and which days are not working
-- days. Balances and requests are not — those are one employee's own record.
CREATE POLICY tenant_read ON leave_absence.leave_types
    FOR SELECT TO authenticated
    USING (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON leave_absence.holidays
    FOR SELECT TO authenticated
    USING (tenant_id = app.current_tenant_id() AND status = 'ACTIVE');

-- ── Grants ───────────────────────────────────────────────────────────────────

-- No DELETE anywhere: a leave request is the record of an absence that was
-- taken or refused, a balance is the running account behind it, and a holiday
-- explains a payslip. All three retire by status or end-date instead.
GRANT SELECT, INSERT, UPDATE ON leave_absence.leave_types    TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON leave_absence.leave_balances TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON leave_absence.leave_requests TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON leave_absence.holidays       TO zoiko_backend;

GRANT SELECT ON leave_absence.leave_types TO authenticated;
GRANT SELECT ON leave_absence.holidays    TO authenticated;
