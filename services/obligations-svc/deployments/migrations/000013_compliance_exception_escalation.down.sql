-- 000013_compliance_exception_escalation.down.sql
-- Obligations Service — Compliance Status, Exception Cases, and Escalation Records (rollback)

BEGIN;

DROP TABLE IF EXISTS escalation_records;
DROP TABLE IF EXISTS exception_cases;
DROP TABLE IF EXISTS compliance_status;

COMMIT;