-- Refused while any observation exists: dropping the table would discard recorded observations.
DO $$
BEGIN
    IF to_regclass('dq_observations') IS NOT NULL AND EXISTS (SELECT 1 FROM dq_observations) THEN
        RAISE EXCEPTION '000007 down refused: observations have been recorded';
    END IF;
END
$$;
DROP INDEX IF EXISTS uq_dq_issues_result;
DROP INDEX IF EXISTS uq_dq_runs_evidence_fingerprint;
ALTER TABLE dq_runs DROP CONSTRAINT IF EXISTS dq_runs_fingerprint_format;
ALTER TABLE dq_runs DROP COLUMN IF EXISTS evidence_fingerprint;
DROP TABLE IF EXISTS dq_observations;
DROP FUNCTION IF EXISTS enforce_dq_observation_immutability();
