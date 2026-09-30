ALTER TABLE emergency_changes DROP CONSTRAINT IF EXISTS chk_emergency_changes_retrospective_closed;
ALTER TABLE emergency_changes
    DROP COLUMN IF EXISTS retrospective_reference,
    DROP COLUMN IF EXISTS prior_value,
    DROP COLUMN IF EXISTS had_prior,
    DROP COLUMN IF EXISTS activated_at,
    DROP COLUMN IF EXISTS activated_row_id,
    DROP COLUMN IF EXISTS kind;
