-- Migration: 000012_pdc05_certification.down.sql
--
-- Refused while any test run exists: runs are the certification evidence of
-- versions that went live on them.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM policy_test_runs) THEN
        RAISE EXCEPTION '000012 down refused: policy_test_runs holds certification evidence';
    END IF;
END
$$;

DROP TABLE IF EXISTS policy_simulation_diffs;
DROP TABLE IF EXISTS policy_simulation_runs;
DROP TABLE IF EXISTS policy_test_waivers;
DROP TABLE IF EXISTS policy_test_runs;
DROP TABLE IF EXISTS policy_test_cases;
DROP FUNCTION IF EXISTS policy_test_cases_guard();
