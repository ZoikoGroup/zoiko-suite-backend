ALTER TABLE control_exceptions DROP COLUMN IF EXISTS expected_clearing;
ALTER TABLE match_results DROP CONSTRAINT IF EXISTS match_results_group_shape;
ALTER TABLE match_results DROP CONSTRAINT IF EXISTS match_results_records_present;
ALTER TABLE match_results DROP CONSTRAINT IF EXISTS match_results_outcome_check;
ALTER TABLE match_results ADD CONSTRAINT match_results_outcome_check CHECK (outcome IN ('EXACT','WITHIN_TOLERANCE'));
ALTER TABLE match_results DROP COLUMN IF EXISTS side_b_records, DROP COLUMN IF EXISTS side_a_records,
    DROP COLUMN IF EXISTS group_reference, DROP COLUMN IF EXISTS kind;
