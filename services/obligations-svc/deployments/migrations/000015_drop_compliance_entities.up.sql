-- 000015_drop_compliance_entities.up.sql
-- Decision D-OB-1 (user, 23 Nov 2026): obligations-svc does not own
-- ComplianceStatus, ExceptionCase or EscalationRecord. Doc 03 gives them to
-- compliance-status-svc (s13.7) and exception-escalation-svc (s13.9), and s18
-- allows one owner per object. The clock escalation of R-001 s9
-- (escalation_policies / escalation_events, 000016) stays here; an overdue
-- clock opens a workflow-svc case and emits obligation.* events those owners
-- consume.
--
-- 000013 created the three tables on 19 Nov 2026 and no service reads them.
-- The down migration recreates them empty.

BEGIN;

DROP TABLE IF EXISTS escalation_records;
DROP TABLE IF EXISTS exception_cases;
DROP TABLE IF EXISTS compliance_status;

COMMIT;
