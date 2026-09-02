-- 0022_employee_master_svc.sql
-- employee-master-svc → schema `employee_master`
--
-- Squashed end state of 000001_initial_schema and 000002_hr_profile_fields.
-- One table: employees.
--
-- ── This service is the employee identity other services resolve against ─────
-- leave-absence-svc, compensation-svc and payroll-run-svc all call
-- GET /v1/employees/{id} to learn which legal entity an employee belongs to,
-- and each fails closed when it cannot. That makes this table the root of the
-- HR domain's scope resolution, which is why it lands before the three
-- migrations that follow it.
--
-- ── Personal data is separated from employment data ──────────────────────────
-- The Go store reads two different projections: a full one for a single
-- employee, and a directory one for a listing that deliberately omits date of
-- birth, gender, personal email and both address fields. A caller enumerating a
-- legal entity is building a directory or a headcount rollup and needs none of
-- them.
--
-- That split is a projection in the service, not a boundary in the schema, so
-- it protects nothing here on its own. The `employee_directory` view below
-- makes it real for anything reaching the table through PostgREST: the
-- `authenticated` role is granted the view and nothing on the base table.
--
-- ── Gender is optional to disclose ───────────────────────────────────────────
-- NULL and 'UNSPECIFIED' mean different things and both are allowed. NULL is
-- "never asked"; UNSPECIFIED is a choice the employee made. Collapsing them
-- would discard the distinction and misreport the second as missing data.

CREATE SCHEMA IF NOT EXISTS employee_master;

COMMENT ON SCHEMA employee_master IS
    'employee-master-svc. Authoritative employee record and the legal-entity scope other HR services resolve against. Holds personal data — expose employee_directory through PostgREST, not the base table.';

GRANT USAGE ON SCHEMA employee_master TO zoiko_backend, authenticated;

-- ── employees ────────────────────────────────────────────────────────────────

CREATE TABLE employee_master.employees (
    employee_id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           VARCHAR(255) NOT NULL,
    legal_entity_id     VARCHAR(255) NOT NULL,

    employee_number     VARCHAR(100) NOT NULL,
    first_name          VARCHAR(100) NOT NULL,
    last_name           VARCHAR(100) NOT NULL,
    email               VARCHAR(255) NOT NULL,
    phone               VARCHAR(50),
    job_title           VARCHAR(150) NOT NULL DEFAULT 'Employee',

    department_id       VARCHAR(255),
    manager_employee_id UUID,

    -- FULL_TIME | PART_TIME | CONTRACTOR
    worker_type         VARCHAR(50)  NOT NULL,
    -- ONBOARDING | ACTIVE | SUSPENDED | TERMINATED
    status              VARCHAR(50)  NOT NULL,

    hire_date           DATE         NOT NULL,
    termination_date    DATE,

    effective_from      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    effective_to        TIMESTAMPTZ,

    -- ── Personal profile ────────────────────────────────────────────────────
    date_of_birth       DATE,
    gender              VARCHAR(30),
    profile_picture_url VARCHAR(500),
    personal_email      VARCHAR(255),
    work_email          VARCHAR(255),

    -- ── Address ─────────────────────────────────────────────────────────────
    current_address     TEXT,
    permanent_address   TEXT,
    city                VARCHAR(100),
    state               VARCHAR(100),
    country             VARCHAR(100),
    postal_code         VARCHAR(20),

    -- ── Org placement ───────────────────────────────────────────────────────
    -- Reporting labels alongside the authoritative department_id.
    -- org-structure-svc owns the real hierarchy; these are the free-text
    -- groupings HR reporting and payroll cost splits need.
    company             VARCHAR(200),
    business_unit       VARCHAR(200),
    division            VARCHAR(200),
    team                VARCHAR(200),
    designation_id      VARCHAR(255),
    confirmation_date   DATE,

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- The vocabulary the domain defines, enforced in Go and — before this —
    -- nowhere else, so any other writer could leave a value no consumer knows
    -- how to read.
    CONSTRAINT employees_worker_type_known
        CHECK (worker_type IN ('FULL_TIME', 'PART_TIME', 'CONTRACTOR')),
    CONSTRAINT employees_status_known
        CHECK (status IN ('ONBOARDING', 'ACTIVE', 'SUSPENDED', 'TERMINATED')),

    -- Optional to disclose, so nullable; a controlled vocabulary when given.
    CONSTRAINT employees_gender_check
        CHECK (gender IS NULL OR gender IN ('MALE', 'FEMALE', 'NON_BINARY', 'OTHER', 'UNSPECIFIED')),

    -- Nobody was confirmed before they were hired.
    CONSTRAINT employees_confirmation_after_hire_check
        CHECK (confirmation_date IS NULL OR confirmation_date >= hire_date),

    -- Nor terminated before they were hired.
    CONSTRAINT employees_termination_after_hire_check
        CHECK (termination_date IS NULL OR termination_date >= hire_date),

    -- A TERMINATED employee must say when. Without this, a terminated row is a
    -- record that somebody left and no account of when — which is the whole
    -- reason downstream payroll reads the status at all.
    CONSTRAINT employees_terminated_has_date
        CHECK (status <> 'TERMINATED' OR termination_date IS NOT NULL),

    -- An employee is not their own manager. A self-reference would make the
    -- reporting chain a cycle of one and hang any recursive walk of it.
    CONSTRAINT employees_manager_not_self
        CHECK (manager_employee_id IS NULL OR manager_employee_id <> employee_id),

    -- The target of the composite self-reference below.
    CONSTRAINT employees_tenant_id_unique UNIQUE (tenant_id, employee_id)
);

-- A manager in another tenant is unrepresentable. The single-column form would
-- have accepted one: nothing made the referenced row's tenant agree with this
-- one's, and a reporting chain that crosses tenants leaks the org structure of
-- both.
ALTER TABLE employee_master.employees
    ADD CONSTRAINT employees_manager_same_tenant
    FOREIGN KEY (tenant_id, manager_employee_id)
    REFERENCES employee_master.employees (tenant_id, employee_id);

CREATE UNIQUE INDEX idx_employees_tenant_email
    ON employee_master.employees (tenant_id, email);
CREATE UNIQUE INDEX idx_employees_tenant_number
    ON employee_master.employees (tenant_id, employee_number);

-- Work email is an addressable identity inside a tenant, so it carries the same
-- uniqueness guarantee as the primary email. Partial: the many rows with no
-- work email must not collide with each other.
CREATE UNIQUE INDEX idx_employees_tenant_work_email
    ON employee_master.employees (tenant_id, work_email)
    WHERE work_email IS NOT NULL;

CREATE INDEX idx_employees_tenant_entity_status
    ON employee_master.employees (tenant_id, legal_entity_id, status);
CREATE INDEX idx_employees_tenant_dept
    ON employee_master.employees (tenant_id, department_id);
CREATE INDEX idx_employees_tenant_manager
    ON employee_master.employees (tenant_id, manager_employee_id);

-- Reporting rollups filter on these; without them a division-level headcount
-- query degrades to a full scan of the tenant's employees.
CREATE INDEX idx_employees_tenant_business_unit
    ON employee_master.employees (tenant_id, business_unit)
    WHERE business_unit IS NOT NULL;
CREATE INDEX idx_employees_tenant_division
    ON employee_master.employees (tenant_id, division)
    WHERE division IS NOT NULL;
CREATE INDEX idx_employees_tenant_designation
    ON employee_master.employees (tenant_id, designation_id)
    WHERE designation_id IS NOT NULL;

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE employee_master.employees ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_master.employees FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON employee_master.employees
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

-- ── employee_directory ───────────────────────────────────────────────────────
--
-- The directory projection, as a view rather than a convention. This is the
-- only employee data `authenticated` can reach: date of birth, gender, personal
-- email and both addresses are absent, and no grant on the base table restores
-- them.
--
-- security_invoker so the view runs under the caller's own privileges and the
-- table's RLS still applies. Without it the view would run as its owner and
-- hand every tenant's directory to every caller.

CREATE VIEW employee_master.employee_directory
WITH (security_invoker = true) AS
SELECT
    employee_id,
    tenant_id,
    legal_entity_id,
    employee_number,
    first_name,
    last_name,
    email,
    work_email,
    phone,
    job_title,
    department_id,
    manager_employee_id,
    worker_type,
    status,
    hire_date,
    termination_date,
    company,
    business_unit,
    division,
    team,
    designation_id,
    confirmation_date,
    created_at,
    updated_at
FROM employee_master.employees;

COMMENT ON VIEW employee_master.employee_directory IS
    'Employment data without personal data. Omits date_of_birth, gender, profile_picture_url, personal_email and both address fields. This is what authenticated callers read; the base table is reachable only by zoiko_backend.';

-- ── Grants ───────────────────────────────────────────────────────────────────

-- No DELETE: an employee record is the basis of every payslip, leave balance
-- and contract that referenced it. Departure is a status change, and
-- effective_to closes the row.
GRANT SELECT, INSERT, UPDATE ON employee_master.employees TO zoiko_backend;

-- Nothing on the base table for authenticated — only the directory view.
GRANT SELECT ON employee_master.employee_directory TO authenticated;
