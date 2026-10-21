-- Migration: 000008_source_journal_checks.down.sql
--
-- Manual rollback only. 000008 only widens the failed_rule CHECK, so there is
-- no column or row to remove. The pre-A6 list cannot validate rows already
-- carrying a SOURCE_* rule, and those rows cannot be deleted (000007's
-- immutability trigger) — dropping them would lose the only evidence for a
-- pair this migration created in MISMATCH. The old list is therefore restored
-- as NOT VALID: a pre-A6 binary only writes the target rules it knows, while
-- the source-rule rows it never writes remain readable. Re-applying 000008
-- later validates all rows again.

ALTER TABLE intercompany_match_attempts
    DROP CONSTRAINT IF EXISTS intercompany_match_attempts_failed_rule_check;

ALTER TABLE intercompany_match_attempts
    ADD CONSTRAINT intercompany_match_attempts_failed_rule_check CHECK (failed_rule IN (
        'COUNTERPARTY_JOURNAL_MISSING', 'TARGET_ENTITY_MISMATCH', 'TARGET_JOURNAL_NOT_FINALIZED',
        'CURRENCY_UNVERIFIABLE', 'CURRENCY_MISMATCH', 'AMOUNT_MISMATCH', 'GROUP_RELATIONSHIP_LOST',
        'LEGACY_UNSTRUCTURED')) NOT VALID;
