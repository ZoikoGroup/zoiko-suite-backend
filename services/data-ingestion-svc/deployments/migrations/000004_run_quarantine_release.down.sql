-- Refused while any release has been recorded: dropping the ledger would discard who released a quarantined batch, and why.
DO $$
BEGIN
    IF to_regclass('quarantine_item_release_ledger') IS NOT NULL AND EXISTS (SELECT 1 FROM quarantine_item_release_ledger) THEN
        RAISE EXCEPTION '000004 down refused: quarantine releases have been recorded';
    END IF;
END
$$;
DROP TABLE IF EXISTS quarantine_item_release_ledger;
DROP FUNCTION IF EXISTS enforce_quarantine_release_append_only();
DROP FUNCTION IF EXISTS enforce_quarantine_release_guard();
