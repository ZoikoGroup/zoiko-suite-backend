-- Migration: 000008_suggestion_candidate_match.up.sql
--
-- SPEC_DEVIATIONS A2a: a suggestion can now propose the book side of an
-- UNMATCHED bank statement line, a FINALIZED general-ledger-svc journal on
-- the line's cash account whose net movement equals the line's amount.
--
--   counterpart_journal_id   the proposed journal. Set only on an AUTO_MATCH
--                            (dated within the governed date tolerance) or a
--                            DATE_SKEW (dated beyond it) proposal.
--   candidate_search_outcome what the search for a counterpart found, stored
--                            on every line that was searched, so "searched,
--                            nothing found" is a recorded result rather than
--                            the absence of one. NULL on a DUPLICATE_ENTRY
--                            copy (a copy is not searched; the entry it
--                            duplicates is) and on rows before this migration.
--   date_tolerance_version   the governed RECONCILIATION_CANDIDATE_DATE_MATCH
--                            tolerance policy version the search used.
--
-- Advisory only: nothing here matches anything. A proposal is confirmed, or
-- not, in bank-reconciliation-svc's own propose/confirm flow, by humans.

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN counterpart_journal_id UUID,
    ADD COLUMN candidate_search_outcome VARCHAR(32),
    ADD COLUMN date_tolerance_version INT;

ALTER TABLE reconciliation_unmatched_items
    ADD CONSTRAINT reconciliation_suggestion_candidate_outcome_check CHECK (
        candidate_search_outcome IS NULL OR candidate_search_outcome IN (
            'WITHIN_DATE_TOLERANCE', 'DATE_SKEW', 'AMBIGUOUS', 'NO_CANDIDATE', 'NO_CASH_ACCOUNT')
    ),
    -- A journal is named exactly when a single counterpart was proposed.
    ADD CONSTRAINT reconciliation_suggestion_counterpart_journal_check CHECK (
        (counterpart_journal_id IS NOT NULL) =
        (candidate_search_outcome IS NOT NULL AND candidate_search_outcome IN ('WITHIN_DATE_TOLERANCE', 'DATE_SKEW'))
    ),
    -- AUTO_MATCH is only ever a proposal of one named journal inside the
    -- governed date tolerance.
    ADD CONSTRAINT reconciliation_suggestion_auto_match_check CHECK (
        recommendation <> 'AUTO_MATCH'
        OR (candidate_search_outcome = 'WITHIN_DATE_TOLERANCE' AND counterpart_journal_id IS NOT NULL)
    ),
    -- A searched line records which date tolerance version it was judged by.
    ADD CONSTRAINT reconciliation_suggestion_date_tolerance_version_check CHECK (
        (candidate_search_outcome IS NULL) = (date_tolerance_version IS NULL)
        AND (date_tolerance_version IS NULL OR date_tolerance_version > 0)
    );
