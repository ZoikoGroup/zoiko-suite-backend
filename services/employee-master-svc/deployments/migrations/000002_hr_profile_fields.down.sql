-- Reverses 000002_hr_profile_fields.

DROP INDEX IF EXISTS idx_employees_tenant_designation;
DROP INDEX IF EXISTS idx_employees_tenant_division;
DROP INDEX IF EXISTS idx_employees_tenant_business_unit;
DROP INDEX IF EXISTS idx_employees_tenant_work_email;

ALTER TABLE employees DROP CONSTRAINT IF EXISTS employees_confirmation_after_hire_check;
ALTER TABLE employees DROP CONSTRAINT IF EXISTS employees_gender_check;

ALTER TABLE employees
    DROP COLUMN IF EXISTS confirmation_date,
    DROP COLUMN IF EXISTS designation_id,
    DROP COLUMN IF EXISTS team,
    DROP COLUMN IF EXISTS division,
    DROP COLUMN IF EXISTS business_unit,
    DROP COLUMN IF EXISTS company,
    DROP COLUMN IF EXISTS postal_code,
    DROP COLUMN IF EXISTS country,
    DROP COLUMN IF EXISTS state,
    DROP COLUMN IF EXISTS city,
    DROP COLUMN IF EXISTS permanent_address,
    DROP COLUMN IF EXISTS current_address,
    DROP COLUMN IF EXISTS work_email,
    DROP COLUMN IF EXISTS personal_email,
    DROP COLUMN IF EXISTS profile_picture_url,
    DROP COLUMN IF EXISTS gender,
    DROP COLUMN IF EXISTS date_of_birth;
