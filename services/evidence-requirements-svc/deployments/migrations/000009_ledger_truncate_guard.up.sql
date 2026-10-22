-- 000009_ledger_truncate_guard.up.sql
-- 000007 made evidence_evaluations append-only for UPDATE and DELETE, but a
-- row-level trigger does not fire on TRUNCATE, so the whole ledger could
-- still be emptied in one statement. A statement-level guard closes it.

CREATE OR REPLACE FUNCTION refuse_evaluation_truncate()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'evidence_evaluations is append-only: TRUNCATE not allowed'
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS evidence_evaluations_no_truncate ON evidence_evaluations;
CREATE TRIGGER evidence_evaluations_no_truncate
    BEFORE TRUNCATE ON evidence_evaluations
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_evaluation_truncate();
