-- Employee Master — HR profile fields
--
-- Adds the personal, address, and org-placement attributes an HR system needs
-- on top of the employment record. Every column is nullable: existing rows stay
-- valid, and a caller that only knows employment data keeps working unchanged.

ALTER TABLE employees
    -- Personal profile
    ADD COLUMN IF NOT EXISTS date_of_birth         DATE,
    ADD COLUMN IF NOT EXISTS gender                VARCHAR(30),
    ADD COLUMN IF NOT EXISTS profile_picture_url   VARCHAR(500),
    ADD COLUMN IF NOT EXISTS personal_email        VARCHAR(255),
    ADD COLUMN IF NOT EXISTS work_email            VARCHAR(255),

    -- Address
    ADD COLUMN IF NOT EXISTS current_address       TEXT,
    ADD COLUMN IF NOT EXISTS permanent_address     TEXT,
    ADD COLUMN IF NOT EXISTS city                  VARCHAR(100),
    ADD COLUMN IF NOT EXISTS state                 VARCHAR(100),
    ADD COLUMN IF NOT EXISTS country               VARCHAR(100),
    ADD COLUMN IF NOT EXISTS postal_code           VARCHAR(20),

    -- Org placement (reporting labels; org-structure-svc owns the real hierarchy)
    ADD COLUMN IF NOT EXISTS company               VARCHAR(200),
    ADD COLUMN IF NOT EXISTS business_unit         VARCHAR(200),
    ADD COLUMN IF NOT EXISTS division              VARCHAR(200),
    ADD COLUMN IF NOT EXISTS team                  VARCHAR(200),
    ADD COLUMN IF NOT EXISTS designation_id        VARCHAR(255),
    ADD COLUMN IF NOT EXISTS confirmation_date     DATE;

-- Gender is a controlled vocabulary, not free text, but stays nullable because
-- it is optional to disclose. UNSPECIFIED is a deliberate choice by the
-- employee; NULL means the field was never asked.
ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS employees_gender_check;
ALTER TABLE employees
    ADD CONSTRAINT employees_gender_check
    CHECK (gender IS NULL OR gender IN ('MALE', 'FEMALE', 'NON_BINARY', 'OTHER', 'UNSPECIFIED'));

-- A confirmed employee cannot have been confirmed before they were hired.
ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS employees_confirmation_after_hire_check;
ALTER TABLE employees
    ADD CONSTRAINT employees_confirmation_after_hire_check
    CHECK (confirmation_date IS NULL OR confirmation_date >= hire_date);

-- Work email is the addressable identity inside a tenant, so it carries the
-- same uniqueness guarantee as the primary email. Partial index: the many rows
-- with no work email must not collide with each other.
CREATE UNIQUE INDEX IF NOT EXISTS idx_employees_tenant_work_email
    ON employees (tenant_id, work_email)
    WHERE work_email IS NOT NULL;

-- Reporting rollups filter on these; without them a division-level headcount
-- query degrades to a full scan of the tenant's employees.
CREATE INDEX IF NOT EXISTS idx_employees_tenant_business_unit
    ON employees (tenant_id, business_unit)
    WHERE business_unit IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_employees_tenant_division
    ON employees (tenant_id, division)
    WHERE division IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_employees_tenant_designation
    ON employees (tenant_id, designation_id)
    WHERE designation_id IS NOT NULL;
