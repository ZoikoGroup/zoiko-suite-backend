-- Refused while any watch row or flag exists: they are the record of what the ledger found resting on a withdrawn certification,
-- and an acknowledged flag is a person's recorded decision.
DO $$
BEGIN
    IF to_regclass('certification_flags') IS NOT NULL AND EXISTS (SELECT 1 FROM certification_flags) THEN
        RAISE EXCEPTION '000036 down refused: certification flags have been raised';
    END IF;
    IF to_regclass('certification_watch') IS NOT NULL AND EXISTS (SELECT 1 FROM certification_watch) THEN
        RAISE EXCEPTION '000036 down refused: certifications are being watched';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_accounting_events_evidence_refs;
DROP INDEX IF EXISTS idx_journal_headers_evidence_refs;
DROP TABLE IF EXISTS certification_flags;
DROP FUNCTION IF EXISTS enforce_certification_flag_rules();
DROP TABLE IF EXISTS certification_watch;
DROP FUNCTION IF EXISTS enforce_certification_watch_rules();
