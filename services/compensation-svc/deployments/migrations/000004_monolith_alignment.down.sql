ALTER TABLE bonus_grants
    DROP COLUMN IF EXISTS paid_at,
    DROP COLUMN IF EXISTS payment_reference,
    DROP COLUMN IF EXISTS payout_period,
    DROP COLUMN IF EXISTS taxable_amount,
    DROP COLUMN IF EXISTS conditions,
    DROP COLUMN IF EXISTS notes;

ALTER TABLE wage_revisions
    DROP COLUMN IF EXISTS previous_amount,
    DROP COLUMN IF EXISTS previous_currency,
    DROP COLUMN IF EXISTS revision_type,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approved_at;

ALTER TABLE compensation_structures
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS grade_code,
    DROP COLUMN IF EXISTS level_code,
    DROP COLUMN IF EXISTS is_default,
    DROP COLUMN IF EXISTS applicable_location;
