-- Migration: 000010_intercompany_line_match_rule.down.sql
--
-- Manual rollback only, same posture as 000008's down: a NO_INTERCOMPANY_LINES
-- row cannot be deleted (000007's immutability trigger) and is real evidence
-- for a pair this migration's forward side put in MISMATCH, so the restored
-- list is NOT VALID rather than re-validated against it. A pre-A5 binary
-- never writes this rule, so it never needs the constraint to allow it.

ALTER TABLE intercompany_match_attempts
    DROP CONSTRAINT IF EXISTS intercompany_match_attempts_failed_rule_check;

ALTER TABLE intercompany_match_attempts
    ADD CONSTRAINT intercompany_match_attempts_failed_rule_check CHECK (failed_rule IN (
        'COUNTERPARTY_JOURNAL_MISSING', 'TARGET_ENTITY_MISMATCH', 'TARGET_JOURNAL_NOT_FINALIZED',
        'CURRENCY_UNVERIFIABLE', 'CURRENCY_MISMATCH', 'AMOUNT_MISMATCH', 'GROUP_RELATIONSHIP_LOST',
        'SOURCE_JOURNAL_MISSING', 'SOURCE_ENTITY_MISMATCH', 'SOURCE_JOURNAL_NOT_FINALIZED',
        'SOURCE_CURRENCY_UNVERIFIABLE', 'SOURCE_CURRENCY_MISMATCH',
        'LEGACY_UNSTRUCTURED')) NOT VALID;
