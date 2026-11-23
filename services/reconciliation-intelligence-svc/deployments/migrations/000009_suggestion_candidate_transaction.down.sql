-- Migration: 000009_suggestion_candidate_transaction.down.sql
--
-- Manual rollback only, refused while any suggestion carries a canonical-
-- transaction search result. Dropping the column would discard which
-- transaction each proposal named, and 000008's CHECKs, restored below,
-- would refuse such a row anyway (an AUTO_MATCH with no journal). The
-- operator decides what to do with them first.

-- The check must see every tenant's rows; see 000008's down migration.
SET LOCAL row_security = off;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM reconciliation_unmatched_items
        WHERE counterpart_transaction_id IS NOT NULL
           OR heuristic_rule IN ('CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE',
                                 'CANDIDATE_TRANSACTION_DATE_SKEW',
                                 'CANDIDATE_TRANSACTION_AMBIGUOUS')
    ) THEN
        RAISE EXCEPTION 'cannot roll back 000009: suggestions with canonical-transaction search results exist';
    END IF;
END
$$;

ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_counterpart_provenance_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_auto_match_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_counterpart_check;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS counterpart_transaction_id;

ALTER TABLE reconciliation_unmatched_items
    ADD CONSTRAINT reconciliation_suggestion_counterpart_journal_check CHECK (
        (counterpart_journal_id IS NOT NULL) =
        (candidate_search_outcome IS NOT NULL AND candidate_search_outcome IN ('WITHIN_DATE_TOLERANCE', 'DATE_SKEW'))
    ),
    ADD CONSTRAINT reconciliation_suggestion_auto_match_check CHECK (
        recommendation <> 'AUTO_MATCH'
        OR (candidate_search_outcome = 'WITHIN_DATE_TOLERANCE' AND counterpart_journal_id IS NOT NULL)
    );
