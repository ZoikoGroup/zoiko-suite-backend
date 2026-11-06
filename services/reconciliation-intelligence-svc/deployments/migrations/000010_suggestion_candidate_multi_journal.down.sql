-- Migration: 000010_suggestion_candidate_multi_journal.down.sql
--
-- Manual rollback only, refused while any suggestion carries a multi-journal
-- search result. Dropping the column would discard which journal pair each
-- proposal named, and 000009's CHECKs, restored below, would refuse such a
-- row anyway (an AUTO_MATCH with no single journal or transaction named).
-- The operator decides what to do with them first.

-- The check must see every tenant's rows; see 000008/000009's down migrations.
SET LOCAL row_security = off;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM reconciliation_unmatched_items
        WHERE counterpart_journal_ids IS NOT NULL
           OR heuristic_rule IN ('CANDIDATE_MULTI_JOURNAL_WITHIN_DATE_TOLERANCE',
                                 'CANDIDATE_MULTI_JOURNAL_DATE_SKEW',
                                 'CANDIDATE_MULTI_JOURNAL_AMBIGUOUS')
    ) THEN
        RAISE EXCEPTION 'cannot roll back 000010: suggestions with multi-journal search results exist';
    END IF;
END
$$;

ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_counterpart_provenance_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_auto_match_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_multi_journal_count_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_counterpart_check;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS counterpart_journal_ids;

ALTER TABLE reconciliation_unmatched_items
    ADD CONSTRAINT reconciliation_suggestion_counterpart_check CHECK (
        num_nonnulls(counterpart_journal_id, counterpart_transaction_id) =
        CASE WHEN candidate_search_outcome IN ('WITHIN_DATE_TOLERANCE', 'DATE_SKEW') THEN 1 ELSE 0 END
    ),
    ADD CONSTRAINT reconciliation_suggestion_auto_match_check CHECK (
        recommendation <> 'AUTO_MATCH'
        OR (candidate_search_outcome = 'WITHIN_DATE_TOLERANCE'
            AND num_nonnulls(counterpart_journal_id, counterpart_transaction_id) = 1)
    ),
    ADD CONSTRAINT reconciliation_suggestion_counterpart_provenance_check CHECK (
        (counterpart_journal_id IS NULL OR heuristic_rule IN (
            'CANDIDATE_JOURNAL_WITHIN_DATE_TOLERANCE', 'CANDIDATE_JOURNAL_DATE_SKEW'))
        AND (counterpart_transaction_id IS NULL OR heuristic_rule IN (
            'CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE', 'CANDIDATE_TRANSACTION_DATE_SKEW'))
    );
