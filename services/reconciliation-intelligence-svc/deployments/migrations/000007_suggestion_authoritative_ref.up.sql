-- Migration: 000007_suggestion_authoritative_ref.up.sql
--
-- A5: an EXECUTED suggestion records the owning domain's record that carried
-- it out, as a structured reference rather than free text in
-- resolution_notes (ZS-CONTROL-001 §21/§24: "Journal/reversal/adjustment/
-- credit/debit note IDs; control does not embed authoritative correction as
-- free text"). The only kind is GL_JOURNAL: a general-ledger-svc journal,
-- by its UUID journal_id.
--
-- Suggestions EXECUTED before this migration recorded no reference, and none
-- is reconstructed. The rule that an EXECUTED WRITE_OFF or TIMING_ADJUSTMENT
-- must carry one is therefore NOT VALID: it binds every write from now on and
-- leaves those rows as they were.

ALTER TABLE reconciliation_unmatched_items
    ADD COLUMN IF NOT EXISTS authoritative_ref_kind VARCHAR(32),
    ADD COLUMN IF NOT EXISTS authoritative_ref_id UUID;

-- A reference is whole or absent, is a GL journal, is recorded only on
-- EXECUTED, and only on a recommendation a journal can carry out.
-- CONFIRM_DUPLICATE and AUTO_MATCH are carried out in
-- bank-reconciliation-svc, not the ledger.
ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_authoritative_ref_check;
ALTER TABLE reconciliation_unmatched_items
    ADD CONSTRAINT reconciliation_suggestion_authoritative_ref_check CHECK (
        (authoritative_ref_kind IS NULL AND authoritative_ref_id IS NULL)
        OR (authoritative_ref_kind = 'GL_JOURNAL'
            AND authoritative_ref_id IS NOT NULL
            AND suggestion_status = 'EXECUTED'
            AND recommendation IN ('WRITE_OFF', 'TIMING_ADJUSTMENT', 'MANUAL_REVIEW'))
    );

-- An accounting action is not EXECUTED without the journal that did it.
ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_accounting_execution_check;
ALTER TABLE reconciliation_unmatched_items
    ADD CONSTRAINT reconciliation_suggestion_accounting_execution_check CHECK (
        suggestion_status <> 'EXECUTED'
        OR recommendation NOT IN ('WRITE_OFF', 'TIMING_ADJUSTMENT')
        OR authoritative_ref_id IS NOT NULL
    ) NOT VALID;
