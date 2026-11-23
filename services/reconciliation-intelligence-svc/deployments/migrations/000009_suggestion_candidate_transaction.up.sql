-- Migration: 000009_suggestion_candidate_transaction.up.sql
--
-- SPEC_DEVIATIONS A2b: when no general-ledger-svc journal matches an
-- UNMATCHED bank statement line, a suggestion can propose a NORMALIZED
-- banking-connector-svc canonical transaction as its counterpart instead.
--
--   counterpart_transaction_id  the proposed canonical transaction. Set only
--                               on an AUTO_MATCH or DATE_SKEW proposal, and
--                               never together with counterpart_journal_id.
--
-- The outcome values are unchanged (WITHIN_DATE_TOLERANCE, DATE_SKEW,
-- AMBIGUOUS, NO_CANDIDATE): they say what the search found. heuristic_rule
-- says which service and kind of record it was found in, and the CHECKs
-- below tie the named counterpart to that rule so provenance can never be
-- ambiguous.
--
-- Advisory only, as in 000008: a proposal is confirmed, or not, in
-- bank-reconciliation-svc's own propose/confirm flow, by humans.

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN counterpart_transaction_id UUID;

ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT reconciliation_suggestion_counterpart_journal_check,
    DROP CONSTRAINT reconciliation_suggestion_auto_match_check;

ALTER TABLE reconciliation_unmatched_items
    -- Exactly one counterpart, journal or transaction, is named exactly when
    -- a single counterpart was proposed.
    ADD CONSTRAINT reconciliation_suggestion_counterpart_check CHECK (
        num_nonnulls(counterpart_journal_id, counterpart_transaction_id) =
        CASE WHEN candidate_search_outcome IN ('WITHIN_DATE_TOLERANCE', 'DATE_SKEW') THEN 1 ELSE 0 END
    ),
    -- AUTO_MATCH is only ever a proposal of one named counterpart inside the
    -- governed date tolerance.
    ADD CONSTRAINT reconciliation_suggestion_auto_match_check CHECK (
        recommendation <> 'AUTO_MATCH'
        OR (candidate_search_outcome = 'WITHIN_DATE_TOLERANCE'
            AND num_nonnulls(counterpart_journal_id, counterpart_transaction_id) = 1)
    ),
    -- The rule names the kind of record that was proposed.
    ADD CONSTRAINT reconciliation_suggestion_counterpart_provenance_check CHECK (
        (counterpart_journal_id IS NULL OR heuristic_rule IN (
            'CANDIDATE_JOURNAL_WITHIN_DATE_TOLERANCE', 'CANDIDATE_JOURNAL_DATE_SKEW'))
        AND (counterpart_transaction_id IS NULL OR heuristic_rule IN (
            'CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE', 'CANDIDATE_TRANSACTION_DATE_SKEW'))
    );
