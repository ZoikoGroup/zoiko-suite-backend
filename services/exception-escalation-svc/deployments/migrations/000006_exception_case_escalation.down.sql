-- 000006_exception_case_escalation.down.sql
-- Exception & Escalation Service — Doc 04 §13.1 (rollback)

BEGIN;

DROP TABLE IF EXISTS escalation_records;
DROP TABLE IF EXISTS exception_cases;

COMMIT;