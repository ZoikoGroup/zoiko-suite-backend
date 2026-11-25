-- Migration: 000010_intercompany_line_match_rule.up.sql
--
-- SPEC_DEVIATIONS A5: MatchEntry now compares the target journal's
-- intercompany-designated line(s) (general-ledger-svc's new
-- IsIntercompanyAccount classification), not the whole journal. A target
-- journal that passes all four journal-level checks (exists, entity,
-- FINALIZED, currency) but has no line posted to an account the chart
-- classifies intercompany has nothing to compare the pair's amount
-- against — a new negative path, recorded the same way CURRENCY_MISMATCH
-- already is: a real accounting fact, not a retryable error.
--
-- Existing rows all carry a rule from 000008's list, so the constraint is
-- validated (not NOT VALID): there is no backfill that could violate it.
ALTER TABLE intercompany_match_attempts
    DROP CONSTRAINT IF EXISTS intercompany_match_attempts_failed_rule_check;

ALTER TABLE intercompany_match_attempts
    ADD CONSTRAINT intercompany_match_attempts_failed_rule_check CHECK (failed_rule IN (
        'COUNTERPARTY_JOURNAL_MISSING', 'TARGET_ENTITY_MISMATCH', 'TARGET_JOURNAL_NOT_FINALIZED',
        'CURRENCY_UNVERIFIABLE', 'CURRENCY_MISMATCH', 'NO_INTERCOMPANY_LINES', 'AMOUNT_MISMATCH',
        'GROUP_RELATIONSHIP_LOST',
        'SOURCE_JOURNAL_MISSING', 'SOURCE_ENTITY_MISMATCH', 'SOURCE_JOURNAL_NOT_FINALIZED',
        'SOURCE_CURRENCY_UNVERIFIABLE', 'SOURCE_CURRENCY_MISMATCH',
        'LEGACY_UNSTRUCTURED'));
