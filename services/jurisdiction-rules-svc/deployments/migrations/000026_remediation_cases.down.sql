-- 000026 down: removes the remediation register, like the down of every migration that created tables.
-- WARNING: this destroys remediation cases and their dispositions, which are the record that someone answered for an outcome made under a withdrawn or
-- rolled-back pack version. Archive them first. It must also run before the down of 000008 (the evidence table they reference).
DROP TABLE IF EXISTS remediation_dispositions;
DROP TABLE IF EXISTS remediation_case_decisions;
DROP TABLE IF EXISTS remediation_cases;
DROP FUNCTION IF EXISTS jur_remediation_case_guard();
DROP FUNCTION IF EXISTS jur_remediation_disposition_guard();
