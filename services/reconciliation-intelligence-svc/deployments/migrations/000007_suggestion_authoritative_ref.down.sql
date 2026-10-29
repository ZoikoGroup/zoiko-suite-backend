-- Migration: 000007_suggestion_authoritative_ref.down.sql
--
-- Manual rollback only. Dropping the columns would discard the only record
-- of which ledger journal carried out each EXECUTED suggestion; nothing
-- else holds it. So the rollback refuses while any reference is recorded,
-- and the operator decides what to do with them first.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_unmatched_items WHERE authoritative_ref_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 000007: recorded authoritative references exist';
    END IF;
END
$$;

ALTER TABLE reconciliation_unmatched_items
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_accounting_execution_check,
    DROP CONSTRAINT IF EXISTS reconciliation_suggestion_authoritative_ref_check;

ALTER TABLE reconciliation_unmatched_items
    DROP COLUMN IF EXISTS authoritative_ref_id,
    DROP COLUMN IF EXISTS authoritative_ref_kind;
