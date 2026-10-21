-- Migration: 000008_source_journal_checks.up.sql
--
-- SPEC_DEVIATIONS A6. CreateIntercompanyPair now runs MatchIntercompany's
-- four target-side checks on the source journal too. A source journal that
-- fails one is a real accounting fact: the pair is created directly in
-- MISMATCH with one intercompany_match_attempts row naming the failed rule.
-- The attempt table's failed_rule CHECK (000007) allowed target-side and
-- legacy rules only; this widens it to the five source-side rules.
--
-- Existing rows all carry a rule from 000007's list, so the constraint is
-- validated (not NOT VALID): there is no backfill that could violate it.

ALTER TABLE intercompany_match_attempts
    DROP CONSTRAINT IF EXISTS intercompany_match_attempts_failed_rule_check;

ALTER TABLE intercompany_match_attempts
    ADD CONSTRAINT intercompany_match_attempts_failed_rule_check CHECK (failed_rule IN (
        'COUNTERPARTY_JOURNAL_MISSING', 'TARGET_ENTITY_MISMATCH', 'TARGET_JOURNAL_NOT_FINALIZED',
        'CURRENCY_UNVERIFIABLE', 'CURRENCY_MISMATCH', 'AMOUNT_MISMATCH', 'GROUP_RELATIONSHIP_LOST',
        'SOURCE_JOURNAL_MISSING', 'SOURCE_ENTITY_MISMATCH', 'SOURCE_JOURNAL_NOT_FINALIZED',
        'SOURCE_CURRENCY_UNVERIFIABLE', 'SOURCE_CURRENCY_MISMATCH',
        'LEGACY_UNSTRUCTURED'));
