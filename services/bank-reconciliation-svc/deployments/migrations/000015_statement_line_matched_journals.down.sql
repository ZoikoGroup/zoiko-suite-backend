-- Refuses while any row represents a CONFIRMED multi-journal match (parent
-- status MATCHED): dropping the table would silently destroy the "explicit
-- allocation" evidence ZS-CONTROL-001 requires for an already-certified
-- fact. A row still tied to a PENDING_CONFIRMATION (merely proposed, not
-- yet confirmed) or EXCEPTION (rejected) line is not evidence of a
-- established fact and does not block the down migration.
--
-- row_security is turned off first: statement_lines is FORCE ROW LEVEL
-- SECURITY, and a NOBYPASSRLS role running this guard must see every
-- tenant's MATCHED lines to refuse correctly, not just its own — the same
-- fix this week's other migrations needed for the identical reason.
SET LOCAL row_security = off;

DO $$
DECLARE
    confirmed_count INT;
BEGIN
    SELECT COUNT(*) INTO confirmed_count
    FROM statement_line_matched_journals j
    JOIN statement_lines l ON l.statement_line_id = j.statement_line_id
    WHERE l.status = 'MATCHED';
    IF confirmed_count > 0 THEN
        RAISE EXCEPTION 'cannot roll back 000015: % statement_line_matched_journals row(s) belong to a CONFIRMED multi-journal match', confirmed_count;
    END IF;
END $$;

DROP TABLE IF EXISTS statement_line_matched_journals;
