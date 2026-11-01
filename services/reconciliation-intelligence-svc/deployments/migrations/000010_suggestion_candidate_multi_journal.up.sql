-- Migration: 000010_suggestion_candidate_multi_journal.up.sql
--
-- SPEC_DEVIATIONS A2b part 2: when no single general-ledger-svc journal
-- matches an UNMATCHED bank statement line, but exactly two journals
-- together sum to it ("one payment booked as two journals"), a suggestion
-- can propose both journals together as the counterpart instead of one.
--
--   counterpart_journal_ids  the two proposed journal ids, as an array. Set
--                            only on an AUTO_MATCH or DATE_SKEW proposal,
--                            and never together with counterpart_journal_id
--                            or counterpart_transaction_id.
--
-- The outcome values are unchanged (WITHIN_DATE_TOLERANCE, DATE_SKEW,
-- AMBIGUOUS, NO_CANDIDATE): they say what the search found. heuristic_rule
-- says which kind of counterpart it was found as, and the CHECKs below tie
-- the named counterpart to that rule, extending 000009's pattern to a third
-- mutually-exclusive counterpart shape.
--
-- Advisory only, as in 000008/000009: a proposal is confirmed, or not, in
-- bank-reconciliation-svc's own propose/confirm flow, by humans — now
-- including its own many-to-one match path (bank-reconciliation-svc
-- migration 000015).

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN counterpart_journal_ids UUID[];

ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT reconciliation_suggestion_counterpart_check,
    DROP CONSTRAINT reconciliation_suggestion_auto_match_check,
    DROP CONSTRAINT reconciliation_suggestion_counterpart_provenance_check;

ALTER TABLE reconciliation_unmatched_items
    -- Exactly one counterpart shape — single journal, canonical transaction,
    -- or the multi-journal array — is named exactly when a single
    -- counterpart was proposed.
    ADD CONSTRAINT reconciliation_suggestion_counterpart_check CHECK (
        (CASE WHEN counterpart_journal_id IS NOT NULL THEN 1 ELSE 0 END
         + CASE WHEN counterpart_transaction_id IS NOT NULL THEN 1 ELSE 0 END
         + CASE WHEN counterpart_journal_ids IS NOT NULL THEN 1 ELSE 0 END) =
        CASE WHEN candidate_search_outcome IN ('WITHIN_DATE_TOLERANCE', 'DATE_SKEW') THEN 1 ELSE 0 END
    ),
    -- The array form names exactly two journals — this increment's scope is
    -- a payment booked as two journals, not an open-ended combination.
    ADD CONSTRAINT reconciliation_suggestion_multi_journal_count_check CHECK (
        counterpart_journal_ids IS NULL OR cardinality(counterpart_journal_ids) = 2
    ),
    -- AUTO_MATCH is only ever a proposal of one named counterpart shape
    -- inside the governed date tolerance.
    ADD CONSTRAINT reconciliation_suggestion_auto_match_check CHECK (
        recommendation <> 'AUTO_MATCH'
        OR (candidate_search_outcome = 'WITHIN_DATE_TOLERANCE'
            AND (CASE WHEN counterpart_journal_id IS NOT NULL THEN 1 ELSE 0 END
                 + CASE WHEN counterpart_transaction_id IS NOT NULL THEN 1 ELSE 0 END
                 + CASE WHEN counterpart_journal_ids IS NOT NULL THEN 1 ELSE 0 END) = 1)
    ),
    -- The rule names the kind of record (or pair of records) that was
    -- proposed.
    ADD CONSTRAINT reconciliation_suggestion_counterpart_provenance_check CHECK (
        (counterpart_journal_id IS NULL OR heuristic_rule IN (
            'CANDIDATE_JOURNAL_WITHIN_DATE_TOLERANCE', 'CANDIDATE_JOURNAL_DATE_SKEW'))
        AND (counterpart_transaction_id IS NULL OR heuristic_rule IN (
            'CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE', 'CANDIDATE_TRANSACTION_DATE_SKEW'))
        AND (counterpart_journal_ids IS NULL OR heuristic_rule IN (
            'CANDIDATE_MULTI_JOURNAL_WITHIN_DATE_TOLERANCE', 'CANDIDATE_MULTI_JOURNAL_DATE_SKEW'))
    );
