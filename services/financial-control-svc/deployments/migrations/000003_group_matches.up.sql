-- ZS-CONTROL-001 Wave 2: explicit many-to-one / one-to-many allocation (§10).
-- A group match records EVERY record allocated on each side, so netting is never invisible.

ALTER TABLE match_results
    ADD COLUMN kind           VARCHAR(12)  NOT NULL DEFAULT 'ONE_TO_ONE' CHECK (kind IN ('ONE_TO_ONE','GROUP')),
    ADD COLUMN group_reference VARCHAR(255) NOT NULL DEFAULT '',
    ADD COLUMN side_a_records TEXT[]       NOT NULL DEFAULT '{}',
    ADD COLUMN side_b_records TEXT[]       NOT NULL DEFAULT '{}';

ALTER TABLE match_results DROP CONSTRAINT match_results_outcome_check;
ALTER TABLE match_results ADD CONSTRAINT match_results_outcome_check
    CHECK (outcome IN ('EXACT','WITHIN_TOLERANCE','GROUP_EXACT','GROUP_WITHIN_TOLERANCE'));

-- Existing one-to-one rows carry their single ids in the arrays too.
-- (match_results is append-only; the trigger is bypassed for this one-time backfill.)
ALTER TABLE match_results DISABLE TRIGGER trg_immutable_match_results;
UPDATE match_results SET side_a_records = ARRAY[side_a_record], side_b_records = ARRAY[side_b_record]
    WHERE cardinality(side_a_records) = 0;
ALTER TABLE match_results ENABLE TRIGGER trg_immutable_match_results;

ALTER TABLE match_results ADD CONSTRAINT match_results_records_present
    CHECK (cardinality(side_a_records) > 0 AND cardinality(side_b_records) > 0);
ALTER TABLE match_results ADD CONSTRAINT match_results_group_shape
    CHECK (kind = 'GROUP' OR (cardinality(side_a_records) = 1 AND cardinality(side_b_records) = 1));

-- Reconciling items carry the date they are expected to clear (§10 timing differences).
ALTER TABLE control_exceptions ADD COLUMN expected_clearing DATE;

-- spec_ref now includes the population params ("gl/account-postings?account_codes=1200&normal_balance=DEBIT").
ALTER TABLE population_snapshots ALTER COLUMN spec_ref TYPE VARCHAR(1024);
