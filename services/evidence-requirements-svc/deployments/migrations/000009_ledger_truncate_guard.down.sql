-- 000009_ledger_truncate_guard.down.sql
DROP TRIGGER IF EXISTS evidence_evaluations_no_truncate ON evidence_evaluations;
DROP FUNCTION IF EXISTS refuse_evaluation_truncate();
