-- Migration: 000009_late_data_checks.down.sql
--
-- Manual rollback only, and refused while any check records a late-data
-- exception it opened. The exceptions themselves stay in control_exceptions
-- either way, but reported_watermark is the only record of which watermark
-- an already-resolved one was opened for: without it, the next sweep after
-- re-applying 000009 would report that same late data again and block the
-- close gate on something already resolved under authority. Checks that
-- opened nothing are derivable again by re-checking, so an environment
-- whose sweep has found no late data can still roll back. The table check
-- keeps this file re-runnable once the table is gone.

DO $$
DECLARE
    reported BOOLEAN := FALSE;
BEGIN
    IF to_regclass('control_watermark_checks') IS NOT NULL THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM control_watermark_checks WHERE late_exception_id IS NOT NULL)'
            INTO reported;
    END IF;
    IF reported THEN
        RAISE EXCEPTION 'cannot roll back 000009: late-data checks that opened an exception exist';
    END IF;
END $$;

DROP TABLE IF EXISTS control_watermark_checks;
