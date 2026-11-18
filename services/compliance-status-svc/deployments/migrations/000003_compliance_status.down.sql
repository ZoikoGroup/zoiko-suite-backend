-- 000003_compliance_status.down.sql
-- Compliance Status Service — Doc 04 §13.1 ComplianceStatus entity (rollback)

BEGIN;

DROP TABLE IF EXISTS compliance_status;

COMMIT;