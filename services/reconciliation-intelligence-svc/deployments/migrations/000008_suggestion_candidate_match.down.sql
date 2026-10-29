-- Migration: 000008_suggestion_candidate_match.down.sql
--
-- Manual rollback only, refused while any suggestion carries a candidate-
-- search result. Dropping the columns would discard which journal each
-- AUTO_MATCH / DATE_SKEW proposal named, which tolerance version judged it,
-- and the record that a line was searched and nothing was found; nothing
-- else holds them. The operator decides what to do with them first.

-- The check must see every tenant's rows. reconciliation_unmatched_items has
-- row-level security enabled (000001): run by a role subject to it, the
-- EXISTS below would see only rows visible to that role and could pass on an
-- empty view. With row_security off, such a role fails the query instead —
-- still a refusal, never a silent loss.
SET LOCAL row_security = off;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM reconciliation_unmatched_items
        WHERE counterpart_journal_id IS NOT NULL
           OR candidate_search_outcome IS NOT NULL
           OR date_tolerance_version IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'cannot roll back 000008: suggestions with candidate-search results exist';
    END IF;
END
$$;

ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_date_tolerance_version_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_auto_match_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_counterpart_journal_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_candidate_outcome_check;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS date_tolerance_version,
    DROP COLUMN IF EXISTS candidate_search_outcome,
    DROP COLUMN IF EXISTS counterpart_journal_id;
